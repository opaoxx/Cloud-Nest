package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

// renameEntryResultStore 在真实更新后注入异常，验证核心服务拒绝结果时的数据库回滚。
type renameEntryResultStore struct {
	*Store
	afterRename func(*domain.FileEntry)
}

func (store *renameEntryResultStore) RenameUserFileEntry(ctx context.Context, entryID, expectedRevision uint64, metadata domain.EntryNameMetadata, nameKey []byte) (*domain.FileEntry, error) {
	entry, err := store.Store.RenameUserFileEntry(ctx, entryID, expectedRevision, metadata, nameKey)
	if err == nil && store.afterRename != nil {
		store.afterRename(entry)
	}
	return entry, err
}

func TestRenameEntryServiceIntegration(t *testing.T) {
	// 1. 使用正式表独占库，建立完整可见路径、跨用户目录和当前登录身份。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表边界错误")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'rename_one','改名服务用户','fixture_hash','ACTIVE'),(3,'rename_three','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','改名服务设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,color_key) VALUES(11,1,NULL,'',X'','DIR',NULL),(33,3,NULL,'',X'','DIR',NULL),(12,1,11,'祖先目录',CAST('祖先目录' AS BINARY),'DIR',NULL),(15,1,12,'普通父目录',CAST('普通父目录' AS BINARY),'DIR','green'),(16,1,15,'同名目标',CAST('同名目标' AS BINARY),'DIR',NULL)",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','rename-service',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,object_id,file_size,revision,mime_type,source_device_id,source_device_name) VALUES(14,1,15,'report.md',CAST('report.md' AS BINARY),'FILE','DOCUMENT','md',41,2048,3,'text/markdown',13,'原始来源设备')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 13, SessionID: 17}
	renamer := service.NewRenameEntryService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RenameEntryTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
	})
	read := func(id uint64) *domain.FileEntry {
		t.Helper()
		entry, err := store.FindUserFileEntry(ctx, 1, id)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	checkFailure := func(t *testing.T, id, revision uint64, name string, want error) {
		t.Helper()
		entry, err := renamer.Rename(ctx, actor, id, revision, name)
		if entry != nil || !errors.Is(err, want) {
			t.Fatal("拒绝结果或错误链错误", err)
		}
	}

	// 2. 单连接串起完整父链和写入，核对文件及目录派生字段。
	t.Run("single connection file and directory", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		entry, err := renamer.Rename(ctx, actor, 14, 3, "  Cafe\u0301.PDF  ")
		if err != nil || entry == nil || entry.Name != "Cafe\u0301.PDF" || entry.Revision != 4 || entry.ParentID == nil || *entry.ParentID != 15 {
			t.Fatal("文件服务结果错误", err)
		}
		display, err := store.FindUserFileEntryDisplay(ctx, 1, 14)
		if err != nil || display.Extension == nil || *display.Extension != "pdf" || display.Category == nil || *display.Category != "DOCUMENT" || display.MIMEType == nil || *display.MIMEType != "text/markdown" || display.SourceDeviceName == nil || *display.SourceDeviceName != "原始来源设备" {
			t.Fatal("文件派生字段或内容事实错误", err)
		}
		var key []byte
		if err := pool.QueryRowContext(ctx, "SELECT name_key FROM file_entries WHERE id=14").Scan(&key); err != nil || string(key) != "café.pdf" {
			t.Fatal("名称键错误", err)
		}
		directory, err := renamer.Rename(ctx, actor, 15, 1, "资料.PDF")
		if err != nil || directory == nil || directory.Revision != 2 {
			t.Fatal("目录服务结果错误", err)
		}
		display, err = store.FindUserFileEntryDisplay(ctx, 1, 15)
		if err != nil || display.Extension != nil || display.Category != nil || display.ColorKey == nil || *display.ColorKey != "green" {
			t.Fatal("目录派生字段或颜色错误", err)
		}
	})
	t.Run("same name version and name conflicts", func(t *testing.T) {
		before := read(14)
		entry, err := renamer.Rename(ctx, actor, 14, before.Revision, "  "+before.Name+"  ")
		if err != nil || !reflect.DeepEqual(entry, before) {
			t.Fatal("同名匹配结果错误", err)
		}
		checkFailure(t, 14, before.Revision-1, before.Name, domain.ErrEntryRevisionConflict)
		checkFailure(t, 14, before.Revision, "同名目标", domain.ErrEntryNameConflict)
		entry, err = renamer.Rename(ctx, actor, 14, before.Revision, "CAFÉ.pdf")
		if err != nil || entry == nil || entry.Revision != before.Revision+1 {
			t.Fatal("规范等价自改名失败", err)
		}
		checkFailure(t, 16, 1, "café.PDF", domain.ErrEntryNameConflict)
	})
	t.Run("scope root and invalid name", func(t *testing.T) {
		checkFailure(t, 999, 1, "新名称", domain.ErrFileEntryNotFound)
		checkFailure(t, 33, 1, "新名称", domain.ErrFileEntryNotFound)
		checkFailure(t, 11, 1, "新名称", domain.ErrRootEntryRename)
		checkFailure(t, 14, read(14).Revision, "CON.txt", domain.ErrInvalidEntryName)
	})
	t.Run("all hidden target and ancestor states", func(t *testing.T) {
		before := read(14)
		for _, hiddenID := range []uint64{14, 15, 12} {
			for _, state := range []string{"TRASHED", "PURGING", "PURGED"} {
				_, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,deleted_at=UTC_TIMESTAMP(6),purge_started_at=IF(?='PURGING',UTC_TIMESTAMP(6),NULL),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL),object_id=IF(entry_type='FILE' AND ?<>'PURGED',41,NULL) WHERE id=?", state, state, state, state, hiddenID)
				if err != nil {
					t.Fatal(err)
				}
				checkFailure(t, 14, before.Revision, "隐藏路径禁止改名", domain.ErrFileEntryNotFound)
				if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL,object_id=IF(entry_type='FILE',41,NULL) WHERE id=?", hiddenID); err != nil {
					t.Fatal(err)
				}
			}
		}
		after := read(14)
		if after.Name != before.Name || after.Revision != before.Revision {
			t.Fatal("路径拒绝后目标发生变化")
		}
	})

	// 3. 服务校验错误和提交前取消须回滚真实名称、版本及派生字段。
	t.Run("bad result and cancellation rollback", func(t *testing.T) {
		for _, mode := range []string{"bad result", "cancel after write"} {
			before := read(14)
			callContext, cancelCall := context.WithCancel(ctx)
			injected := service.NewRenameEntryService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RenameEntryTransaction) error) error {
				return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
					return action(txContext, &renameEntryResultStore{Store: txStore, afterRename: func(entry *domain.FileEntry) {
						if mode == "bad result" {
							entry.Revision++
						} else {
							cancelCall()
						}
					}})
				})
			})
			entry, err := injected.Rename(callContext, actor, 14, before.Revision, "回滚.ZIP")
			cancelCall()
			want := domain.ErrInvalidFileEntryData
			if mode == "cancel after write" {
				want = context.Canceled
			}
			if entry != nil || !errors.Is(err, want) || !reflect.DeepEqual(read(14), before) {
				t.Fatal("服务异常未完整回滚", err)
			}
			display, err := store.FindUserFileEntryDisplay(ctx, 1, 14)
			if err != nil || display.Category == nil || *display.Category != "DOCUMENT" || display.Extension == nil || *display.Extension != "pdf" {
				t.Fatal("派生字段未回滚", err)
			}
		}
	})
	t.Run("identity recheck", func(t *testing.T) {
		for _, testCase := range []struct {
			change, restore string
			want            error
		}{
			{"UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
			{"UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=13", "UPDATE user_devices SET revoked_at=NULL WHERE id=13", domain.ErrDeviceRevoked},
			{"UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		} {
			before := read(14)
			if _, err := pool.ExecContext(ctx, testCase.change); err != nil {
				t.Fatal(err)
			}
			entry, operationError := renamer.Rename(ctx, actor, 14, before.Revision, "身份失效请求")
			if _, err := pool.ExecContext(ctx, testCase.restore); err != nil {
				t.Fatal(err)
			}
			if entry != nil || !errors.Is(operationError, testCase.want) || !reflect.DeepEqual(read(14), before) {
				t.Fatal("身份复查错误", operationError)
			}
		}
	})
	t.Run("same revision concurrent requests", func(t *testing.T) {
		before := read(14)
		var wait sync.WaitGroup
		results := make(chan error, 2)
		for index := 0; index < 2; index++ {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				entry, err := renamer.Rename(ctx, actor, 14, before.Revision, fmt.Sprintf("并发改名%d.md", index))
				if err == nil && (entry == nil || entry.Revision != before.Revision+1) {
					err = errors.New("并发成功结果错误")
				}
				results <- err
			}(index)
		}
		wait.Wait()
		close(results)
		succeeded, conflicts := 0, 0
		for err := range results {
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, domain.ErrEntryRevisionConflict):
				conflicts++
			default:
				t.Fatal(err)
			}
		}
		if succeeded != 1 || conflicts != 1 || read(14).Revision != before.Revision+1 {
			t.Fatal("并发版本保护错误")
		}
	})
	t.Run("maximum allowed depth", func(t *testing.T) {
		parentID := uint64(11)
		for depth := 1; depth <= domain.MaxFileEntryDepth; depth++ {
			name, entryID := fmt.Sprintf("depth%d", depth), uint64(100+depth)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,?,?,?,'DIR')", entryID, parentID, name, []byte(name)); err != nil {
				t.Fatal(err)
			}
			parentID = entryID
		}
		entry, err := renamer.Rename(ctx, actor, parentID, 1, "第三十二层")
		if err != nil || entry == nil || entry.Revision != 2 {
			t.Fatal("合法最大深度改名失败", err)
		}
	})
}
