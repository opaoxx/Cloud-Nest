package repository

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestRenameUserFileEntryIntegration(t *testing.T) {
	// 1. 使用正式表独占库，准备当前身份、文件、目录和偏好事实。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for _, boundary := range []struct{ table, next string }{
		{"auth_sessions", "stored_objects"}, {"user_entry_preferences", "upload_tasks"},
	} {
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
		"INSERT INTO users(id,username,display_name,password_hash,status,storage_quota,used_storage) VALUES(1,'fixture_one','目录项改名用户','fixture_hash','ACTIVE',4096,2048),(3,'fixture_three','其他用户','fixture_hash','ACTIVE',4096,0)",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','改名设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-entry-rename',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,color_key) VALUES(11,1,NULL,'',X'','DIR',NULL),(33,3,NULL,'',X'','DIR',NULL),(15,1,11,'资料',CAST('资料' AS BINARY),'DIR','green'),(16,1,11,'同名目标',CAST('同名目标' AS BINARY),'DIR',NULL)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(19,1,11,'回收重名',CAST('回收重名' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,object_id,file_size,revision,mime_type,source_device_id,source_device_name) VALUES(12,1,11,'report.md',CAST('report.md' AS BINARY),'FILE','DOCUMENT','md',41,2048,3,'text/markdown',13,'内容来源设备')",
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
	read := func(t *testing.T, id uint64) *domain.FileEntry {
		t.Helper()
		entry, err := store.FindUserFileEntry(ctx, 1, id)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	rename := func(callContext context.Context, id, revision uint64, name string) (*domain.FileEntry, error) {
		current, err := store.FindUserFileEntry(callContext, actor.UserID, id)
		entryType := "DIR"
		if err == nil {
			entryType = current.EntryType
		}
		metadata, err := service.BuildEntryNameMetadata(entryType, name)
		if err != nil {
			return nil, err
		}
		key, err := service.BuildEntryNameKey(metadata.Name)
		if err != nil {
			return nil, err
		}
		var prepared *domain.FileEntry
		err = store.WithUserTransaction(callContext, actor, func(txContext context.Context, txStore *Store) error {
			var operationError error
			prepared, operationError = txStore.RenameUserFileEntry(txContext, id, revision, *metadata, key)
			return operationError
		})
		if err != nil {
			return nil, err
		}
		return prepared, nil
	}

	// 2. 实际文件与目录更新，核对派生字段、内容事实、偏好和无变化路径。
	t.Run("single connection file metadata and directory", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		before := read(t, 12)
		entry, err := rename(ctx, 12, 3, "  Report.PDF  ")
		if err != nil || entry.Name != "Report.PDF" || entry.Revision != 4 || !entry.CreatedAt.Equal(before.CreatedAt) || !entry.ModifiedAt.After(before.ModifiedAt) || !entry.ModifiedAt.Equal(entry.UpdatedAt) || entry.FileSize != 2048 || *entry.ParentID != 11 {
			t.Fatal("文件改名基础字段或时间错误", err)
		}
		display, err := store.FindUserFileEntryDisplay(ctx, 1, 12)
		if err != nil || display.Category == nil || *display.Category != "DOCUMENT" || display.Extension == nil || *display.Extension != "pdf" || *display.MIMEType != "text/markdown" || *display.SourceDeviceID != 13 || *display.SourceDeviceName != "内容来源设备" || !display.ContentModifiedAt.Equal(oldStamp) {
			t.Fatal("派生字段或内容事实错误", err)
		}
		var objectID, usedStorage uint64
		var starred, opened time.Time
		if err := pool.QueryRowContext(ctx, "SELECT object_id FROM file_entries WHERE id=12").Scan(&objectID); err != nil || objectID != 41 {
			t.Fatal("内容对象事实错误", err)
		}
		if err := pool.QueryRowContext(ctx, "SELECT used_storage FROM users WHERE id=1").Scan(&usedStorage); err != nil || usedStorage != 2048 {
			t.Fatal("配额事实错误", err)
		}
		if err := pool.QueryRowContext(ctx, "SELECT starred_at,last_opened_at FROM user_entry_preferences WHERE user_id=1 AND entry_id=12").Scan(&starred, &opened); err != nil || !starred.Equal(oldStamp) || !opened.Equal(oldStamp) {
			t.Fatal("偏好事实错误", err)
		}
		directory, err := rename(ctx, 15, 1, "资料.PDF")
		if err != nil || directory.Revision != 2 {
			t.Fatal("目录改名错误", err)
		}
		display, err = store.FindUserFileEntryDisplay(ctx, 1, 15)
		if err != nil || display.Category != nil || display.Extension != nil || display.ColorKey == nil || *display.ColorKey != "green" {
			t.Fatal("目录派生值或配色事实错误", err)
		}
	})
	t.Run("same name and stale version", func(t *testing.T) {
		before := read(t, 12)
		entry, err := rename(ctx, 12, before.Revision, before.Name)
		if err != nil || !reflect.DeepEqual(before, entry) {
			t.Fatal("相同名称未返回当前事实", err)
		}
		if entry, err := rename(ctx, 12, before.Revision-1, before.Name); entry != nil || !errors.Is(err, domain.ErrEntryRevisionConflict) {
			t.Fatal("同名请求绕过版本检查", err)
		}
	})
	t.Run("name conflicts and canonical self rename", func(t *testing.T) {
		before := read(t, 15)
		if entry, err := rename(ctx, 15, before.Revision, "同名目标"); entry != nil || !errors.Is(err, domain.ErrEntryNameConflict) || !reflect.DeepEqual(before, read(t, 15)) {
			t.Fatal("重名错误或失败回滚错误", err)
		}
		file := read(t, 12)
		file, err = rename(ctx, 12, file.Revision, "REPORT.pdf")
		if err != nil {
			t.Fatal("仅大小写变化被误判重名", err)
		}
		file, err = rename(ctx, 12, file.Revision, "Cafe\u0301")
		if err != nil {
			t.Fatal(err)
		}
		file, err = rename(ctx, 12, file.Revision, "CAFÉ")
		if err != nil || file.Name != "CAFÉ" {
			t.Fatal("规范等价自改名错误", err)
		}
		if entry, err := rename(ctx, 15, before.Revision, "café"); entry != nil || !errors.Is(err, domain.ErrEntryNameConflict) {
			t.Fatal("跨类型规范等价重名未拒绝", err)
		}
		if _, err := rename(ctx, 15, before.Revision, "回收重名"); err != nil {
			t.Fatal("历史同名项影响活动名称复用", err)
		}
	})
	t.Run("scope hidden and root", func(t *testing.T) {
		for _, testCase := range []struct {
			id   uint64
			want error
		}{{999, domain.ErrFileEntryNotFound}, {33, domain.ErrFileEntryNotFound}, {19, domain.ErrFileEntryNotFound}, {11, domain.ErrRootEntryRename}} {
			if entry, err := rename(ctx, testCase.id, 1, "新名称"); entry != nil || !errors.Is(err, testCase.want) {
				t.Fatal("目标范围、状态或根目录规则错误", testCase.id, err)
			}
		}
	})

	// 3. 在旧快照中使用当前锁定读，失败与取消交回真实事务回滚。
	t.Run("current locking read over old snapshot", func(t *testing.T) {
		before := read(t, 12)
		metadata, err := service.BuildEntryNameMetadata("FILE", "当前锁定名.md")
		if err != nil {
			t.Fatal(err)
		}
		key, err := service.BuildEntryNameKey(metadata.Name)
		if err != nil {
			t.Fatal(err)
		}
		err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if _, err := txStore.FindUserFileEntry(txContext, 1, 12); err != nil {
				return err
			}
			// 外部测试连接更新记录，制造事务一致性快照与当前事实的差异。
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name=?,name_key=?,revision=revision+1 WHERE id=12", metadata.Name, key); err != nil {
				return err
			}
			entry, err := txStore.RenameUserFileEntry(txContext, 12, before.Revision+1, *metadata, key)
			if err != nil {
				return err
			}
			if entry.Name != metadata.Name || entry.Revision != before.Revision+1 {
				return errors.New("未取得当前锁定读的无变化结果")
			}
			return nil
		})
		if err != nil {
			t.Fatal("旧快照覆盖了当前锁定读", err)
		}
	})
	t.Run("rollback and cancel before commit", func(t *testing.T) {
		for _, cancelBeforeCommit := range []bool{false, true} {
			before := read(t, 12)
			metadata, err := service.BuildEntryNameMetadata("FILE", "回滚名称.png")
			if err != nil {
				t.Fatal(err)
			}
			key, err := service.BuildEntryNameKey(metadata.Name)
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("fixture outer rollback")
			callContext, cancelCall := context.WithCancel(ctx)
			err = store.WithUserTransaction(callContext, actor, func(txContext context.Context, txStore *Store) error {
				if _, err := txStore.RenameUserFileEntry(txContext, 12, before.Revision, *metadata, key); err != nil {
					return err
				}
				if cancelBeforeCommit {
					cancelCall()
					return callContext.Err()
				}
				return failure
			})
			cancelCall()
			want := failure
			if cancelBeforeCommit {
				want = context.Canceled
			}
			if !errors.Is(err, want) || !reflect.DeepEqual(before, read(t, 12)) {
				t.Fatal("实际改名未按外层错误回滚", err)
			}
			display, err := store.FindUserFileEntryDisplay(ctx, 1, 12)
			if err != nil || display.Category == nil || *display.Category != "OTHER" || display.Extension != nil {
				t.Fatal("回滚后派生字段错误", err)
			}
		}
	})
	t.Run("identity recheck", func(t *testing.T) {
		for _, mutation := range []struct {
			change, restore string
			want            error
		}{
			{"UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
			{"UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=13", "UPDATE user_devices SET revoked_at=NULL WHERE id=13", domain.ErrDeviceRevoked},
			{"UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		} {
			before := read(t, 12)
			if _, err := pool.ExecContext(ctx, mutation.change); err != nil {
				t.Fatal(err)
			}
			entry, callError := rename(ctx, 12, before.Revision, "身份失效.md")
			if _, err := pool.ExecContext(ctx, mutation.restore); err != nil {
				t.Fatal(err)
			}
			if entry != nil || !errors.Is(callError, mutation.want) || !reflect.DeepEqual(before, read(t, 12)) {
				t.Fatal("身份复查失败仍改名", callError)
			}
		}
	})

	// 4. 用户锁协调同版本竞争和唯一名称竞争，完整无符号版本在真实 SQL 中递增。
	t.Run("same version concurrency", func(t *testing.T) {
		before := read(t, 12)
		results := make(chan error, 2)
		start := make(chan struct{})
		for _, name := range []string{"并发版本一.md", "并发版本二.md"} {
			go func() { <-start; _, err := rename(ctx, 12, before.Revision, name); results <- err }()
		}
		close(start)
		first, second := <-results, <-results
		if !((first == nil && errors.Is(second, domain.ErrEntryRevisionConflict)) || (second == nil && errors.Is(first, domain.ErrEntryRevisionConflict))) || read(t, 12).Revision != before.Revision+1 {
			t.Fatal("同版本并发结果错误", first, second)
		}
	})
	t.Run("same name concurrency", func(t *testing.T) {
		firstTarget, secondTarget := read(t, 15), read(t, 16)
		results := make(chan error, 2)
		start := make(chan struct{})
		for _, target := range []*domain.FileEntry{firstTarget, secondTarget} {
			go func() { <-start; _, err := rename(ctx, target.ID, target.Revision, "并发同名"); results <- err }()
		}
		close(start)
		first, second := <-results, <-results
		if !((first == nil && errors.Is(second, domain.ErrEntryNameConflict)) || (second == nil && errors.Is(first, domain.ErrEntryNameConflict))) {
			t.Fatal("唯一名称并发结果错误", first, second)
		}
	})
	t.Run("high unsigned id and revision limit", func(t *testing.T) {
		const highID uint64 = 9223372036854776000
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,object_id,file_size,revision) VALUES(?,1,11,'高位.md',CAST('高位.md' AS BINARY),'FILE','DOCUMENT','md',41,2048,?)", highID, maximum-1); err != nil {
			t.Fatal(err)
		}
		entry, err := rename(ctx, highID, maximum-1, "高位.pdf")
		if err != nil || entry.ID != highID || entry.Revision != maximum {
			t.Fatal("完整无符号标识或版本递增错误", err)
		}
		if _, err := rename(ctx, highID, maximum, "高位.pdf"); err != nil {
			t.Fatal("最大版本的同名提交失败", err)
		}
		if result, err := rename(ctx, highID, maximum, "高位.txt"); result != nil || !errors.Is(err, domain.ErrEntryRevisionExhausted) || !reflect.DeepEqual(entry, read(t, highID)) {
			t.Fatal("最大版本实际更新未按上限拒绝", err)
		}
	})
}
