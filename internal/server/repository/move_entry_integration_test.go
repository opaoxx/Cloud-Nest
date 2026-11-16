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

func TestMoveUserFileEntryIntegration(t *testing.T) {
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
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,color_key) VALUES(11,1,NULL,'',X'','DIR',NULL),(33,3,NULL,'',X'','DIR',NULL),(15,1,11,'源目录',CAST('源目录' AS BINARY),'DIR',NULL),(16,1,11,'目的地',CAST('目的地' AS BINARY),'DIR',NULL),(18,1,11,'其他目的地',CAST('其他目的地' AS BINARY),'DIR',NULL),(19,1,15,'待移动目录',CAST('待移动目录' AS BINARY),'DIR','green'),(20,1,19,'后代目录',CAST('后代目录' AS BINARY),'DIR',NULL),(21,1,18,'REPORT.md',CAST('report.md' AS BINARY),'DIR',NULL)",
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
	move := func(callContext context.Context, source, revision, target uint64) (*domain.FileEntry, error) {
		var prepared *domain.FileEntry
		err := store.WithUserTransaction(callContext, actor, func(txContext context.Context, txStore *Store) error {
			checker := service.NewEntryMoveTargetService(func(ctx context.Context, action func(context.Context, service.FileEntryPathReader) error) error {
				return action(ctx, txStore)
			})
			paths, err := checker.Check(txContext, actor, source, target)
			if err != nil {
				return err
			}
			stats, err := txStore.FindUserEntrySubtreeStats(txContext, actor.UserID, source)
			if err != nil {
				return err
			}
			if err := service.ValidateEntryMoveLimits(paths, stats); err != nil {
				return err
			}
			prepared, err = txStore.MoveUserFileEntry(txContext, source, revision, target)
			return err
		})
		if err != nil {
			return nil, err
		}
		return prepared, nil
	}
	// 2. 完整前置检查和移动写入复用同一连接，核对元数据与内容事实。
	t.Run("single connection file and directory", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		entry, err := move(ctx, 12, 3, 16)
		if err != nil || entry == nil || entry.ParentID == nil || *entry.ParentID != 16 || entry.Revision != 4 || entry.Name != "report.md" || entry.FileSize != 2048 || !entry.CreatedAt.Equal(oldStamp) || !entry.ModifiedAt.After(oldStamp) || !entry.UpdatedAt.Equal(entry.ModifiedAt) {
			t.Fatal("实际文件移动结果错误", err)
		}
		var key []byte
		var objectID, used uint64
		var mime string
		var contentAt time.Time
		if err := pool.QueryRowContext(ctx, "SELECT e.name_key,e.object_id,e.mime_type,e.content_modified_at,u.used_storage FROM file_entries e JOIN users u ON u.id=e.user_id WHERE e.id=12").Scan(&key, &objectID, &mime, &contentAt, &used); err != nil || string(key) != "report.md" || objectID != 41 || mime != "text/markdown" || !contentAt.Equal(oldStamp) || used != 2048 {
			t.Fatal("内容、名称键或配额事实错误", err)
		}
		preference, err := store.FindUserEntryPreference(ctx, 1, 12)
		if err != nil || preference.StarredAt == nil || !preference.StarredAt.Equal(oldStamp) || preference.LastOpenedAt == nil || !preference.LastOpenedAt.Equal(oldStamp) {
			t.Fatal("移动后偏好事实错误", err)
		}
		folder, err := move(ctx, 19, 1, 16)
		if err != nil || folder == nil || folder.ParentID == nil || *folder.ParentID != 16 || folder.Revision != 2 {
			t.Fatal("目录移动错误", err)
		}
		display, err := store.FindUserFileEntryDisplay(ctx, 1, 19)
		if err != nil || display.ColorKey == nil || *display.ColorKey != "green" {
			t.Fatal("目录颜色错误", err)
		}
		paths := service.NewFileEntryPathService(func(ctx context.Context, action func(context.Context, service.FileEntryPathReader) error) error {
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error { return action(snapshotContext, txStore) })
		})
		path, err := paths.Get(ctx, actor, 20)
		if err != nil || len(path.Entries) != 4 || path.Entries[1].ID != 16 || path.Entries[2].ID != 19 || read(20).Revision != 1 {
			t.Fatal("后代新路径事实错误", err)
		}
		*folder.ParentID = 999
		if *read(19).ParentID != 16 {
			t.Fatal("修改返回记录影响了数据库")
		}
	})
	t.Run("same position conflict and protected targets", func(t *testing.T) {
		before := read(12)
		if entry, err := move(ctx, 12, before.Revision, 16); err != nil || !reflect.DeepEqual(entry, before) {
			t.Fatal("同位置匹配错误", err)
		}
		for _, item := range []struct {
			source, revision, target uint64
			want                     error
		}{
			{12, 3, 16, domain.ErrEntryRevisionConflict}, {12, before.Revision, 18, domain.ErrEntryNameConflict},
			{999, 1, 16, domain.ErrFileEntryNotFound}, {12, before.Revision, 999, domain.ErrFileEntryNotFound}, {33, 1, 16, domain.ErrFileEntryNotFound}, {12, before.Revision, 33, domain.ErrFileEntryNotFound},
			{11, 1, 16, domain.ErrRootEntryMove}, {12, before.Revision, 12, domain.ErrFileEntryNotDirectory}, {19, 2, 20, domain.ErrEntryMoveIntoSubtree},
		} {
			if entry, err := move(ctx, item.source, item.revision, item.target); entry != nil || !errors.Is(err, item.want) {
				t.Fatal("移动规则或错误类别错误", err)
			}
		}
		if !reflect.DeepEqual(before, read(12)) {
			t.Fatal("失败请求改变了源事实")
		}
	})
	t.Run("outer error and cancellation rollback", func(t *testing.T) {
		failure := errors.New("fixture outer move rollback")
		for _, cancelBeforeCommit := range []bool{false, true} {
			before := read(12)
			callContext, cancelCall := context.WithCancel(ctx)
			err := store.WithUserTransaction(callContext, actor, func(txContext context.Context, txStore *Store) error {
				if _, err := txStore.MoveUserFileEntry(txContext, 12, before.Revision, 15); err != nil {
					return err
				}
				if cancelBeforeCommit {
					cancelCall()
					return nil
				}
				return failure
			})
			cancelCall()
			want := failure
			if cancelBeforeCommit {
				want = context.Canceled
			}
			if !errors.Is(err, want) || !reflect.DeepEqual(before, read(12)) {
				t.Fatal("实际移动未回滚", err)
			}
		}
	})
	t.Run("current locked record over old snapshot", func(t *testing.T) {
		before := read(12)
		nameKey, err := service.BuildEntryNameKey("锁定读名称.md")
		if err != nil {
			t.Fatal(err)
		}
		var current *domain.FileEntry
		err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if _, err := txStore.FindUserFileEntry(txContext, 1, 12); err != nil {
				return err
			}
			if _, err := pool.ExecContext(txContext, "UPDATE file_entries SET name='锁定读名称.md',name_key=?,revision=revision+1 WHERE id=12", nameKey); err != nil {
				return err
			}
			var err error
			current, err = txStore.MoveUserFileEntry(txContext, 12, before.Revision+1, 16)
			return err
		})
		if err != nil || current == nil || current.Name != "锁定读名称.md" || current.Revision != before.Revision+1 {
			t.Fatal("同位置返回了旧快照事实", err)
		}
	})
	t.Run("identity recheck", func(t *testing.T) {
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
			entry, operationError := move(ctx, 12, before.Revision, 15)
			if _, err := pool.ExecContext(ctx, item.restore); err != nil {
				t.Fatal(err)
			}
			if entry != nil || !errors.Is(operationError, item.want) || !reflect.DeepEqual(before, read(12)) {
				t.Fatal("移动绕过身份复查", operationError)
			}
		}
	})
	// 3. 同版本竞争与活动名称竞争由用户事务和唯一键协调。
	t.Run("same revision concurrency", func(t *testing.T) {
		before := read(12)
		results := make(chan error, 2)
		for _, target := range []uint64{15, 18} {
			go func() { _, err := move(ctx, 12, before.Revision, target); results <- err }()
		}
		successes, conflicts := 0, 0
		for index := 0; index < 2; index++ {
			err := <-results
			switch {
			case err == nil:
				successes++
			case errors.Is(err, domain.ErrEntryRevisionConflict):
				conflicts++
			default:
				t.Fatal(err)
			}
		}
		if successes != 1 || conflicts != 1 || read(12).Revision != before.Revision+1 {
			t.Fatal("并发版本保护错误")
		}
	})
	t.Run("same destination name concurrency", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(24,1,15,'竞争名称',CAST('竞争名称' AS BINARY),'DIR'),(25,1,18,'竞争名称',CAST('竞争名称' AS BINARY),'DIR')"); err != nil {
			t.Fatal(err)
		}
		results := make(chan error, 2)
		for _, source := range []uint64{24, 25} {
			go func() { _, err := move(ctx, source, 1, 16); results <- err }()
		}
		successes, conflicts := 0, 0
		for index := 0; index < 2; index++ {
			err := <-results
			switch {
			case err == nil:
				successes++
			case errors.Is(err, domain.ErrEntryNameConflict):
				conflicts++
			default:
				t.Fatal(err)
			}
		}
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_entries WHERE user_id=1 AND parent_id=16 AND name='竞争名称' AND status='ACTIVE'").Scan(&count); err != nil || count != 1 || successes != 1 || conflicts != 1 {
			t.Fatal("活动名称竞争错误", err)
		}
	})
	t.Run("maximum entry destination and revision", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,revision) VALUES(?,1,11,'高位源',CAST('高位源' AS BINARY),'DIR',?),(?,1,11,'高位目的地',CAST('高位目的地' AS BINARY),'DIR',1)", maximum, maximum-1, maximum-1); err != nil {
			t.Fatal(err)
		}
		entry, err := move(ctx, maximum, maximum-1, 16)
		if err != nil || entry == nil || entry.Revision != maximum || entry.ParentID == nil || *entry.ParentID != 16 {
			t.Fatal("最大源和版本移动错误", err)
		}
		if same, err := move(ctx, maximum, maximum, 16); err != nil || !reflect.DeepEqual(same, entry) {
			t.Fatal("最大版本同位置匹配错误", err)
		}
		if result, err := move(ctx, maximum, maximum, 15); result != nil || !errors.Is(err, domain.ErrEntryRevisionExhausted) {
			t.Fatal("最大版本实际移动未拒绝", err)
		}
		folder, err := move(ctx, 19, 2, maximum-1)
		if err != nil || folder == nil || folder.ParentID == nil || *folder.ParentID != maximum-1 || folder.Revision != 3 {
			t.Fatal("最大目的地移动错误", err)
		}
	})
}
