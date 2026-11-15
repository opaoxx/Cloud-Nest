package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// fileEntryPathChangingReader 在一次真实快照读取后执行受控的并发变更。
type fileEntryPathChangingReader struct {
	*Store
	afterRead func(uint64) error
}

func (reader *fileEntryPathChangingReader) FindUserFileEntry(ctx context.Context, userID, entryID uint64) (*domain.FileEntry, error) {
	entry, err := reader.Store.FindUserFileEntry(ctx, userID, entryID)
	if err == nil && reader.afterRead != nil {
		err = reader.afterRead(entryID)
	}
	return entry, err
}

func TestFileEntryPathServiceIntegration(t *testing.T) {
	// 1. 正式表独占库中装配真实查询、用户资料检查和只读快照适配器。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','路径用户','fixture_hash','ACTIVE'),(2,'fixture_two','禁用用户','fixture_hash','DISABLED'),(3,'fixture_three','其他用户','fixture_hash','ACTIVE'),(4,'fixture_four','缺少根目录','fixture_hash','ACTIVE')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-path-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(22,2,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'课程资料',CAST('课程资料' AS BINARY),'DIR'),(14,1,11,'其他目录',CAST('其他目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(13,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048,3),(42,1,11,'其他文件.md',CAST('其他文件.md' AS BINARY),'FILE','DOCUMENT',41,2048,1)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("创建路径服务夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	withSnapshot := func(ctx context.Context, action func(context.Context, service.FileEntryPathReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error {
			return action(snapshotContext, txStore)
		})
	}
	paths := service.NewFileEntryPathService(withSnapshot)
	actor := domain.Actor{UserID: 1, SessionID: 17, DeviceID: 19}
	checkPath := func(t *testing.T, result *domain.FileEntryPath, expected []uint64) {
		t.Helper()
		if result == nil || len(result.Entries) != len(expected) {
			t.Fatal("真实路径长度错误")
		}
		for index, entry := range result.Entries {
			if entry.ID != expected[index] || entry.UserID != actor.UserID || entry.Status != "ACTIVE" {
				t.Fatal("真实路径顺序、归属或状态错误")
			}
			if index == 0 {
				if entry.ParentID != nil || entry.Name != "" || entry.EntryType != "DIR" {
					t.Fatal("路径未从真实根目录开始")
				}
			} else if entry.ParentID == nil || *entry.ParentID != expected[index-1] {
				t.Fatal("路径父子关系错误")
			}
		}
	}

	// 2. 核对正常路径、目标/祖先的历史状态及账号归属。
	t.Run("root directory file and independent copies", func(t *testing.T) {
		for _, expected := range [][]uint64{{11}, {11, 12}, {11, 12, 13}} {
			result, err := paths.Get(ctx, actor, expected[len(expected)-1])
			if err != nil {
				t.Fatal(err)
			}
			checkPath(t, result, expected)
		}
		first, err := paths.Get(ctx, actor, 13)
		if err != nil || first == nil {
			t.Fatal(err)
		}
		second, err := paths.Get(ctx, actor, 13)
		if err != nil || second == nil || !reflect.DeepEqual(first, second) || second.Entries[2].FileSize != 2048 || second.Entries[2].Revision != 3 {
			t.Fatal("真实路径字段或重复结果错误", err)
		}
		first.Entries[2].Name, *first.Entries[2].ParentID = "本地修改", 99
		checkPath(t, second, []uint64{11, 12, 13})
	})
	t.Run("hidden target and ancestors", func(t *testing.T) {
		for _, targetID := range []uint64{12, 13} {
			for _, status := range []string{"TRASHED", "PURGING", "PURGED"} {
				t.Run(fmt.Sprintf("%d/%s", targetID, status), func(t *testing.T) {
					objectID := any(nil)
					if targetID == 13 && status != "PURGED" {
						objectID = uint64(41)
					}
					if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,deleted_at=UTC_TIMESTAMP(6),purge_started_at=UTC_TIMESTAMP(6),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL),object_id=? WHERE id=?", status, status, objectID, targetID); err != nil {
						t.Fatal(err)
					}
					defer func() {
						var originalObject any
						if targetID == 13 {
							originalObject = uint64(41)
						}
						if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL,object_id=? WHERE id=?", originalObject, targetID); err != nil {
							t.Error("恢复路径夹具失败", err)
						}
					}()
					if result, err := paths.Get(ctx, actor, 13); result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
						t.Fatal("真实回收或清理中的路径仍可见", err)
					}
				})
			}
		}
	})
	t.Run("ownership missing account and disabled account", func(t *testing.T) {
		for _, testCase := range []struct {
			userID, entryID uint64
			want            error
		}{
			{userID: 1, entryID: 33, want: domain.ErrFileEntryNotFound},
			{userID: 3, entryID: 13, want: domain.ErrFileEntryNotFound},
			{userID: 1, entryID: 999, want: domain.ErrFileEntryNotFound},
			{userID: 2, entryID: 22, want: domain.ErrAccountDisabled},
			{userID: 4, entryID: 13, want: domain.ErrInvalidAccountData},
			{userID: 99, entryID: 13, want: domain.ErrAccountNotFound},
		} {
			identity := actor
			identity.UserID = testCase.userID
			if result, err := paths.Get(ctx, identity, testCase.entryID); result != nil || !errors.Is(err, testCase.want) {
				t.Fatal("真实归属或账号错误分类异常", err)
			}
		}
	})
	t.Run("cycles and file parent rejected", func(t *testing.T) {
		for _, statements := range [][]string{
			{"UPDATE file_entries SET parent_id=12 WHERE id=12"},
			{"UPDATE file_entries SET parent_id=14 WHERE id=12", "UPDATE file_entries SET parent_id=12 WHERE id=14"},
			{"UPDATE file_entries SET parent_id=42 WHERE id=13"},
		} {
			for _, statement := range statements {
				if _, err := pool.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			result, err := paths.Get(ctx, actor, 13)
			if result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("真实循环或文件父项未拒绝", err)
			}
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=IF(id=13,12,11) WHERE id IN (12,13,14)"); err != nil {
				t.Fatal(err)
			}
		}
	})

	// 3. 并发提交移动与回收时，本次路径仍使用同一个读取快照。
	t.Run("concurrent move and trash use one snapshot", func(t *testing.T) {
		changing := service.NewFileEntryPathService(func(ctx context.Context, action func(context.Context, service.FileEntryPathReader) error) error {
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error {
				reader := &fileEntryPathChangingReader{Store: txStore, afterRead: func(entryID uint64) error {
					if entryID != 13 {
						return nil
					}
					writer, err := pool.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					defer writer.Rollback()
					if _, err := writer.ExecContext(ctx, "UPDATE file_entries SET parent_id=14,revision=revision+1 WHERE id=13"); err != nil {
						return err
					}
					if _, err := writer.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=12"); err != nil {
						return err
					}
					return writer.Commit()
				}}
				return action(snapshotContext, reader)
			})
		})
		result, err := changing.Get(ctx, actor, 13)
		if err != nil {
			t.Fatal(err)
		}
		checkPath(t, result, []uint64{11, 12, 13})
		current, err := paths.Get(ctx, actor, 13)
		if err != nil {
			t.Fatal(err)
		}
		checkPath(t, current, []uint64{11, 14, 13})
		if result, err := paths.Get(ctx, actor, 12); result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("新快照未拒绝已回收目录", err)
		}
	})
	t.Run("snapshot failure discards prepared path", func(t *testing.T) {
		failure := errors.New("fixture before snapshot end")
		failing := service.NewFileEntryPathService(func(ctx context.Context, action func(context.Context, service.FileEntryPathReader) error) error {
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error {
				if err := action(snapshotContext, txStore); err != nil {
					return err
				}
				return failure
			})
		})
		if result, err := failing.Get(ctx, actor, 13); result != nil || !errors.Is(err, failure) {
			t.Fatal("快照失败仍交付已暂存路径", err)
		}
	})

	// 4. 按根目录深度 0 验证 32 层边界，再核对完整无符号整数。
	t.Run("depth boundary", func(t *testing.T) {
		parentID := uint64(11)
		for depth := 1; depth <= 33; depth++ {
			entryID := uint64(100 + depth)
			name := fmt.Sprintf("level-%d", depth)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,?,?,?,'DIR')", entryID, parentID, name, []byte(name)); err != nil {
				t.Fatal(err)
			}
			parentID = entryID
		}
		if result, err := paths.Get(ctx, actor, 132); err != nil || result == nil || len(result.Entries) != 33 {
			t.Fatal("真实第 32 层路径被拒绝", err)
		}
		if result, err := paths.Get(ctx, actor, 133); result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
			t.Fatal("真实超过 32 层路径未拒绝", err)
		}
	})
	t.Run("full unsigned identifiers", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','最大用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum-1, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,revision) VALUES(?,?,?,'最大目录',CAST('最大目录' AS BINARY),'DIR',?)", maximum, maximum, maximum-1, maximum); err != nil {
			t.Fatal(err)
		}
		identity := domain.Actor{UserID: maximum, SessionID: maximum, DeviceID: maximum}
		result, err := paths.Get(ctx, identity, maximum)
		if err != nil || result == nil || len(result.Entries) != 2 || result.Entries[0].ID != maximum-1 || result.Entries[1].ID != maximum || result.Entries[1].Revision != maximum || *result.Entries[1].ParentID != maximum-1 {
			t.Fatal("真实完整整数路径错误", err)
		}
	})
}
