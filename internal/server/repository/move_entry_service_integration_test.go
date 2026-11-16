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

func TestMoveEntryServiceIntegration(t *testing.T) {
	// 1. 正式表独占库中绑定身份，准备源、目的地、内容和偏好事实。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, boundary := range []struct{ table, next string }{{"auth_sessions", "stored_objects"}, {"user_entry_preferences", "upload_tasks"}} {
		start := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.table+" (")
		end := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.next+" (")
		if start < 0 || end <= start {
			t.Fatal("正式表边界错误")
		}
		if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status,storage_quota,used_storage) VALUES(1,'move_writer','移动写入用户','fixture_hash','ACTIVE',4096,2048),(3,'move_other','其他用户','fixture_hash','ACTIVE',4096,0)",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','移动设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','move-entry-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,color_key) VALUES(11,1,NULL,'',X'','DIR',NULL),(33,3,NULL,'',X'','DIR',NULL),(15,1,11,'源目录',CAST('源目录' AS BINARY),'DIR',NULL),(25,1,11,'目的地祖先',CAST('目的地祖先' AS BINARY),'DIR',NULL),(16,1,25,'目的地',CAST('目的地' AS BINARY),'DIR',NULL),(18,1,11,'其他目的地',CAST('其他目的地' AS BINARY),'DIR',NULL),(19,1,15,'待移动目录',CAST('待移动目录' AS BINARY),'DIR','green'),(20,1,19,'后代目录',CAST('后代目录' AS BINARY),'DIR',NULL),(21,1,18,'REPORT.md',CAST('report.md' AS BINARY),'DIR',NULL)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(22,1,16,'report.md',CAST('report.md' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,object_id,file_size,revision,mime_type,source_device_id,source_device_name) VALUES(12,1,15,'report.md',CAST('report.md' AS BINARY),'FILE','DOCUMENT','md',41,2048,3,'text/markdown',13,'原内容来源设备')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	oldStamp := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Microsecond)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?,content_modified_at=IF(entry_type='FILE',?,NULL)", oldStamp, oldStamp, oldStamp, oldStamp); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,12,?,?)", oldStamp, oldStamp); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 13, SessionID: 17}
	read := func(id uint64) *domain.FileEntry {
		t.Helper()
		entry, err := store.FindUserFileEntry(ctx, 1, id)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	mover := service.NewMoveEntryService(func(ctx context.Context, current domain.Actor, action func(context.Context, service.MoveEntryTransaction) error) error {
		return store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
	})
	checkFailure := func(t *testing.T, source, revision, target uint64, want error) {
		t.Helper()
		entry, err := mover.Move(ctx, actor, source, revision, target)
		if entry != nil || !errors.Is(err, want) {
			t.Fatal("移动规则或错误链错误", err)
		}
	}

	// 2. 单连接完成路径、统计、限制和写入，读取完整展示及偏好事实。
	t.Run("single connection file and directory", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		entry, err := mover.Move(ctx, actor, 12, 3, 16)
		if err != nil || entry == nil || entry.ParentID == nil || *entry.ParentID != 16 || entry.Revision != 4 || !entry.CreatedAt.Equal(oldStamp) || !entry.ModifiedAt.After(oldStamp) {
			t.Fatal("文件移动结果错误", err)
		}
		display, err := store.FindUserFileEntryDisplay(ctx, 1, 12)
		if err != nil || display.Extension == nil || *display.Extension != "md" || display.MIMEType == nil || *display.MIMEType != "text/markdown" || display.SourceDeviceID == nil || *display.SourceDeviceID != 13 || display.SourceDeviceName == nil || *display.SourceDeviceName != "原内容来源设备" || display.ContentModifiedAt == nil || !display.ContentModifiedAt.Equal(oldStamp) {
			t.Fatal("移动后文件展示事实错误", err)
		}
		preference, err := store.FindUserEntryPreference(ctx, 1, 12)
		if err != nil || preference.StarredAt == nil || !preference.StarredAt.Equal(oldStamp) || preference.LastOpenedAt == nil || !preference.LastOpenedAt.Equal(oldStamp) {
			t.Fatal("移动后偏好事实错误", err)
		}
		var objectID, size, used uint64
		var key []byte
		if err := pool.QueryRowContext(ctx, "SELECT e.object_id,e.file_size,e.name_key,u.used_storage FROM file_entries e JOIN users u ON u.id=e.user_id WHERE e.id=12").Scan(&objectID, &size, &key, &used); err != nil || objectID != 41 || size != 2048 || used != 2048 || string(key) != "report.md" {
			t.Fatal("内容引用、大小、名称键或配额事实错误", err)
		}
		folder, err := mover.Move(ctx, actor, 19, 1, 16)
		if err != nil || folder == nil || folder.Revision != 2 || *folder.ParentID != 16 {
			t.Fatal("目录移动结果错误", err)
		}
		paths := service.NewFileEntryPathService(func(ctx context.Context, action func(context.Context, service.FileEntryPathReader) error) error {
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error { return action(snapshotContext, txStore) })
		})
		path, err := paths.Get(ctx, actor, 20)
		if err != nil || len(path.Entries) != 5 || path.Entries[1].ID != 25 || path.Entries[2].ID != 16 || path.Entries[3].ID != 19 || read(20).Revision != 1 {
			t.Fatal("后代新路径错误", err)
		}
		*folder.ParentID = 999
		if *read(19).ParentID != 16 {
			t.Fatal("修改返回记录影响数据库")
		}
	})
	t.Run("same position version scope and relationships", func(t *testing.T) {
		before := read(12)
		entry, err := mover.Move(ctx, actor, 12, before.Revision, 16)
		if err != nil || !reflect.DeepEqual(entry, before) {
			t.Fatal("同位置事实错误", err)
		}
		for _, item := range []struct {
			source, revision, target uint64
			want                     error
		}{
			{12, 3, 16, domain.ErrEntryRevisionConflict}, {12, before.Revision, 18, domain.ErrEntryNameConflict},
			{999, 1, 16, domain.ErrFileEntryNotFound}, {12, before.Revision, 999, domain.ErrFileEntryNotFound},
			{33, 1, 16, domain.ErrFileEntryNotFound}, {12, before.Revision, 33, domain.ErrFileEntryNotFound},
			{11, 1, 16, domain.ErrRootEntryMove}, {12, before.Revision, 12, domain.ErrFileEntryNotDirectory},
			{19, 2, 19, domain.ErrEntryMoveIntoSubtree}, {19, 2, 20, domain.ErrEntryMoveIntoSubtree},
		} {
			checkFailure(t, item.source, item.revision, item.target, item.want)
		}
		if !reflect.DeepEqual(read(12), before) {
			t.Fatal("失败请求改变源记录")
		}
	})
	t.Run("hidden source and destination chains", func(t *testing.T) {
		for _, id := range []uint64{12, 16, 25} {
			for _, status := range []string{"TRASHED", "PURGING", "PURGED"} {
				if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,deleted_at=UTC_TIMESTAMP(6),purge_started_at=IF(?='TRASHED',NULL,UTC_TIMESTAMP(6)),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL),object_id=IF(entry_type='FILE' AND ?='PURGED',NULL,object_id) WHERE id=?", status, status, status, status, id); err != nil {
					t.Fatal(err)
				}
				entry, operationError := mover.Move(ctx, actor, 12, 4, 15)
				if entry != nil || !errors.Is(operationError, domain.ErrFileEntryNotFound) {
					t.Fatal("隐藏源路径未短路", operationError)
				}
				if id == 16 || id == 25 {
					// 源文件此时位于 16，另外用目录 21 验证目的地链。
					entry, operationError = mover.Move(ctx, actor, 21, 1, 16)
				}
				if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL,object_id=IF(entry_type='FILE',41,NULL) WHERE id=?", id); err != nil {
					t.Fatal(err)
				}
				if entry != nil || !errors.Is(operationError, domain.ErrFileEntryNotFound) {
					t.Fatal("隐藏路径未短路", operationError)
				}
			}
		}
	})
	t.Run("result rejection and cancellation rollback", func(t *testing.T) {
		for _, cancelAfterWrite := range []bool{false, true} {
			before := read(12)
			callContext, cancelCall := context.WithCancel(ctx)
			broken := service.NewMoveEntryService(func(ctx context.Context, current domain.Actor, action func(context.Context, service.MoveEntryTransaction) error) error {
				return store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
					return action(txContext, &moveEntryResultStore{Store: txStore, afterMove: func(entry *domain.FileEntry) {
						if cancelAfterWrite {
							cancelCall()
						} else {
							entry.Name = "错误读回名称"
						}
					}})
				})
			})
			entry, operationError := broken.Move(callContext, actor, 12, before.Revision, 15)
			cancelCall()
			want := domain.ErrInvalidFileEntryData
			if cancelAfterWrite {
				want = context.Canceled
			}
			if entry != nil || !errors.Is(operationError, want) || !reflect.DeepEqual(before, read(12)) {
				t.Fatal("实际写后失败未回滚", operationError)
			}
		}
	})
	t.Run("authenticated user transaction rechecks", func(t *testing.T) {
		for _, item := range []struct {
			change, restore string
			want            error
		}{
			{"UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
			{"UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=13", "UPDATE user_devices SET revoked_at=NULL WHERE id=13", domain.ErrDeviceRevoked},
			{"UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		} {
			before := read(12)
			if _, err := pool.ExecContext(ctx, item.change); err != nil {
				t.Fatal(err)
			}
			entry, operationError := mover.Move(ctx, actor, 12, before.Revision, 15)
			if _, err := pool.ExecContext(ctx, item.restore); err != nil {
				t.Fatal(err)
			}
			if entry != nil || !errors.Is(operationError, item.want) || !reflect.DeepEqual(before, read(12)) {
				t.Fatal("身份复查错误", operationError)
			}
		}
	})
	t.Run("concurrent same revision", func(t *testing.T) {
		before := read(12)
		var wait sync.WaitGroup
		results := make(chan error, 2)
		for _, target := range []uint64{11, 15} {
			wait.Add(1)
			go func(target uint64) {
				defer wait.Done()
				entry, err := mover.Move(ctx, actor, 12, before.Revision, target)
				if err == nil && (entry == nil || entry.Revision != before.Revision+1 || *entry.ParentID != target) {
					err = errors.New("并发成功结果错误")
				}
				results <- err
			}(target)
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
		if succeeded != 1 || conflicts != 1 || read(12).Revision != before.Revision+1 {
			t.Fatal("并发版本保护错误")
		}
	})
	t.Run("actual final depth limits", func(t *testing.T) {
		parentID := uint64(11)
		for depth := 1; depth <= 32; depth++ {
			name, id := fmt.Sprintf("depth%d", depth), uint64(100+depth)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,?,?,?,'DIR')", id, parentID, name, []byte(name)); err != nil {
				t.Fatal(err)
			}
			parentID = id
		}
		before := read(12)
		entry, err := mover.Move(ctx, actor, 12, before.Revision, 131)
		if err != nil || entry == nil || *entry.ParentID != 131 {
			t.Fatal("文件最终 32 层失败", err)
		}
		checkFailure(t, 12, entry.Revision, 132, domain.ErrFileEntryDepthExceeded)
		checkFailure(t, 19, 2, 131, domain.ErrFileEntryDepthExceeded)
		folder, err := mover.Move(ctx, actor, 19, 2, 130)
		if err != nil || folder == nil || *folder.ParentID != 130 || folder.Revision != 3 {
			t.Fatal("目录子树最终 32 层失败", err)
		}
	})
	t.Run("actual subtree count limits", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(300,1,11,'规模目录',CAST('规模目录' AS BINARY),'DIR')"); err != nil {
			t.Fatal(err)
		}
		for offset := 0; offset < 9999; offset += 500 {
			values := []string{}
			arguments := []any{}
			for index := offset; index < offset+500 && index < 9999; index++ {
				name := fmt.Sprintf("child%d", index)
				values = append(values, "(?,1,300,?,?,'DIR')")
				arguments = append(arguments, 1000+index, name, []byte(name))
			}
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES"+strings.Join(values, ","), arguments...); err != nil {
				t.Fatal(err)
			}
		}
		entry, err := mover.Move(ctx, actor, 300, 1, 15)
		if err != nil || entry == nil || entry.Revision != 2 || *entry.ParentID != 15 {
			t.Fatal("10000 项实际移动失败", err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(20000,1,300,'extra',X'6578747261','DIR')"); err != nil {
			t.Fatal(err)
		}
		checkFailure(t, 300, 2, 16, domain.ErrFileEntrySubtreeTooLarge)
		checkFailure(t, 300, 2, 15, domain.ErrFileEntrySubtreeTooLarge)
		if !reflect.DeepEqual(entry, read(300)) {
			t.Fatal("超限请求改变源记录")
		}
	})
	t.Run("maximum identifiers and revision", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,revision) VALUES(?,1,11,'最大目的地',CAST('最大目的地' AS BINARY),'DIR',1),(?,1,11,'最大源',CAST('最大源' AS BINARY),'DIR',?)", maximum-1, maximum, maximum-1); err != nil {
			t.Fatal(err)
		}
		entry, err := mover.Move(ctx, actor, maximum, maximum-1, maximum-1)
		if err != nil || entry == nil || entry.ID != maximum || entry.Revision != maximum || *entry.ParentID != maximum-1 {
			t.Fatal("完整标识或最大版本递增错误", err)
		}
		same, err := mover.Move(ctx, actor, maximum, maximum, maximum-1)
		if err != nil || !reflect.DeepEqual(entry, same) {
			t.Fatal("最大版本同位置错误", err)
		}
		checkFailure(t, maximum, maximum, 11, domain.ErrEntryRevisionExhausted)
	})
}

// moveEntryResultStore 在真实更新后注入取消或错误结果，核对核心服务的回滚边界。
type moveEntryResultStore struct {
	*Store
	afterMove func(*domain.FileEntry)
}

func (store *moveEntryResultStore) MoveUserFileEntry(ctx context.Context, entryID, expectedRevision, targetParentID uint64) (*domain.FileEntry, error) {
	entry, err := store.Store.MoveUserFileEntry(ctx, entryID, expectedRevision, targetParentID)
	if err == nil && store.afterMove != nil {
		store.afterMove(entry)
	}
	return entry, err
}
