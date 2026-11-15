package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// directoryListChangingReader 在目录路径读取期间触发受控并发变更。
type directoryListChangingReader struct {
	*Store
	afterRead func(uint64) error
	listCalls int
}

func (reader *directoryListChangingReader) FindUserFileEntry(ctx context.Context, userID, entryID uint64) (*domain.FileEntry, error) {
	entry, err := reader.Store.FindUserFileEntry(ctx, userID, entryID)
	if err == nil && reader.afterRead != nil {
		err = reader.afterRead(entryID)
	}
	return entry, err
}

func (reader *directoryListChangingReader) ListUserDirectoryEntries(ctx context.Context, userID, parentID uint64, before *domain.FileEntryPosition, pageSize int) (*domain.FileEntryPage, error) {
	reader.listCalls++
	return reader.Store.ListUserDirectoryEntries(ctx, userID, parentID, before, pageSize)
}

func TestDirectoryListServiceIntegration(t *testing.T) {
	// 1. 正式表独占库中装配路径读取、子项分页和同一次快照。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','列表服务用户','fixture_hash','ACTIVE'),(2,'fixture_two','禁用用户','fixture_hash','DISABLED'),(3,'fixture_three','其他用户','fixture_hash','ACTIVE'),(4,'fixture_four','缺根用户','fixture_hash','ACTIVE')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-list-service',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(22,2,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(14,1,11,'祖先目录',CAST('祖先目录' AS BINARY),'DIR'),(15,1,11,'其他目录',CAST('其他目录' AS BINARY),'DIR'),(12,1,14,'当前目录',CAST('当前目录' AS BINARY),'DIR'),(102,1,12,'子目录',CAST('子目录' AS BINARY),'DIR'),(103,1,12,'旧子目录',CAST('旧子目录' AS BINARY),'DIR'),(301,3,33,'其他账号子项',CAST('其他账号子项' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(101,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048,3),(42,1,11,'根下文件.md',CAST('根下文件.md' AS BINARY),'FILE','DOCUMENT',41,2048,1)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(110,1,12,'已回收',CAST('已回收' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at) VALUES(111,1,12,'清理中',CAST('清理中' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,file_size,status,purged_at) VALUES(112,1,12,'已清理',CAST('已清理' AS BINARY),'FILE','OTHER',64,'PURGED',UTC_TIMESTAMP(6))",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("创建列表服务夹具失败", err)
		}
	}
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", stamp.Add(-time.Hour), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=? WHERE id IN (101,102)", stamp.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	lists := service.NewDirectoryListService(func(ctx context.Context, action func(context.Context, service.DirectoryListReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error { return action(snapshotContext, txStore) })
	})
	actor := domain.Actor{UserID: 1, SessionID: 17, DeviceID: 19}
	check := func(t *testing.T, result *domain.DirectoryList, pathIDs, entryIDs []uint64, more bool) {
		t.Helper()
		if result == nil || result.Entries == nil || len(result.Path.Entries) != len(pathIDs) || len(result.Entries) != len(entryIDs) || result.HasMore != more {
			t.Fatal("目录路径或页形状错误")
		}
		for index, entry := range result.Path.Entries {
			if entry.ID != pathIDs[index] || entry.UserID != actor.UserID || entry.Status != "ACTIVE" || entry.EntryType != "DIR" {
				t.Fatal("路径顺序或可见性错误")
			}
		}
		parentID := pathIDs[len(pathIDs)-1]
		for index, entry := range result.Entries {
			if entry.ID != entryIDs[index] || entry.UserID != actor.UserID || entry.ParentID == nil || *entry.ParentID != parentID || entry.Status != "ACTIVE" {
				t.Fatal("子项顺序或归属错误")
			}
		}
	}

	// 2. 验证真实根目录、空目录、分页、字段副本与范围拒绝。
	t.Run("root empty and paged directory", func(t *testing.T) {
		root, err := lists.List(ctx, actor, 11, nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		check(t, root, []uint64{11}, []uint64{42, 15, 14}, false)
		empty, err := lists.List(ctx, actor, 15, nil, 2)
		if err != nil {
			t.Fatal(err)
		}
		check(t, empty, []uint64{11, 15}, nil, false)
		first, err := lists.List(ctx, actor, 12, nil, 2)
		if err != nil {
			t.Fatal(err)
		}
		check(t, first, []uint64{11, 14, 12}, []uint64{102, 101}, true)
		if first.Entries[1].FileSize != 2048 || first.Entries[1].Revision != 3 || first.Entries[1].Name != "学习笔记.md" {
			t.Fatal("文件字段丢失")
		}
		last := first.Entries[1]
		second, err := lists.List(ctx, actor, 12, &domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID}, 2)
		if err != nil {
			t.Fatal(err)
		}
		check(t, second, []uint64{11, 14, 12}, []uint64{103}, false)
		first.Path.Entries[1].Name, first.Entries[0].Name, *first.Entries[0].ParentID = "本地路径", "本地子项", 99
		again, err := lists.List(ctx, actor, 12, nil, 2)
		if err != nil {
			t.Fatal(err)
		}
		check(t, again, []uint64{11, 14, 12}, []uint64{102, 101}, true)
	})
	t.Run("parent and ancestor must be visible", func(t *testing.T) {
		for _, targetID := range []uint64{12, 14} {
			for _, status := range []string{"TRASHED", "PURGING", "PURGED"} {
				t.Run(fmt.Sprintf("%d/%s", targetID, status), func(t *testing.T) {
					if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,deleted_at=UTC_TIMESTAMP(6),purge_started_at=UTC_TIMESTAMP(6),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL) WHERE id=?", status, status, targetID); err != nil {
						t.Fatal(err)
					}
					defer func() {
						if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL WHERE id=?", targetID); err != nil {
							t.Error(err)
						}
					}()
					var reader *directoryListChangingReader
					checked := service.NewDirectoryListService(func(ctx context.Context, action func(context.Context, service.DirectoryListReader) error) error {
						return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error {
							reader = &directoryListChangingReader{Store: txStore}
							return action(snapshotContext, reader)
						})
					})
					result, err := checked.List(ctx, actor, 12, nil, 2)
					if result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) || reader == nil || reader.listCalls != 0 {
						t.Fatal("不可见父链仍读取了子项", err)
					}
				})
			}
		}
	})
	t.Run("file parent and account isolation", func(t *testing.T) {
		for _, testCase := range []struct {
			userID, parentID uint64
			want             error
		}{
			{1, 42, domain.ErrFileEntryNotDirectory}, {1, 33, domain.ErrFileEntryNotFound}, {3, 12, domain.ErrFileEntryNotFound}, {1, 999, domain.ErrFileEntryNotFound}, {2, 22, domain.ErrAccountDisabled}, {4, 12, domain.ErrInvalidAccountData}, {99, 12, domain.ErrAccountNotFound},
		} {
			identity := actor
			identity.UserID = testCase.userID
			if result, err := lists.List(ctx, identity, testCase.parentID, nil, 2); result != nil || !errors.Is(err, testCase.want) {
				t.Fatal("文件父项或账号归属错误", err)
			}
		}
	})

	// 3. 路径检查期间并发移动父目录、回收祖先并变更子项，两部分仍使用同一快照。
	t.Run("path and page share one snapshot", func(t *testing.T) {
		snapshots := 0
		changing := service.NewDirectoryListService(func(ctx context.Context, action func(context.Context, service.DirectoryListReader) error) error {
			snapshots++
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error {
				reader := &directoryListChangingReader{Store: txStore, afterRead: func(entryID uint64) error {
					if entryID != 12 {
						return nil
					}
					writer, err := pool.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					defer writer.Rollback()
					if _, err := writer.ExecContext(ctx, "UPDATE file_entries SET parent_id=15,revision=revision+1 WHERE id=12"); err != nil {
						return err
					}
					if _, err := writer.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id IN (14,102)"); err != nil {
						return err
					}
					if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,modified_at) VALUES(104,1,12,'新子项',CAST('新子项' AS BINARY),'DIR',?)", stamp.Add(5*time.Second)); err != nil {
						return err
					}
					return writer.Commit()
				}}
				return action(snapshotContext, reader)
			})
		})
		original, err := changing.List(ctx, actor, 12, nil, 2)
		if err != nil || snapshots != 1 {
			t.Fatal("目录组合未使用单次快照", err)
		}
		check(t, original, []uint64{11, 14, 12}, []uint64{102, 101}, true)
		current, err := lists.List(ctx, actor, 12, nil, 2)
		if err != nil {
			t.Fatal(err)
		}
		check(t, current, []uint64{11, 15, 12}, []uint64{104, 101}, true)
		if result, err := lists.List(ctx, actor, 14, nil, 2); result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("新快照仍允许回收祖先", err)
		}
	})
	t.Run("snapshot failure and cancellation discard result", func(t *testing.T) {
		for _, mode := range []string{"failure", "cancel"} {
			t.Run(mode, func(t *testing.T) {
				requestContext, stop := context.WithCancel(ctx)
				defer stop()
				failure := errors.New("fixture end")
				failing := service.NewDirectoryListService(func(ctx context.Context, action func(context.Context, service.DirectoryListReader) error) error {
					return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error {
						if err := action(snapshotContext, txStore); err != nil {
							return err
						}
						if mode == "cancel" {
							stop()
							return nil
						}
						return failure
					})
				})
				want := failure
				if mode == "cancel" {
					want = context.Canceled
				}
				if result, err := failing.List(requestContext, actor, 12, nil, 2); result != nil || !errors.Is(err, want) {
					t.Fatal("快照失败仍交付目录列表", err)
				}
				if _, err := lists.List(ctx, actor, 12, nil, 2); err != nil {
					t.Fatal("故障后快照不可复用", err)
				}
			})
		}
	})

	// 4. 核对边界深度和完整整数，最大主键夹具放在普通夹具之后。
	t.Run("maximum depth directory and children", func(t *testing.T) {
		parentID := uint64(11)
		for depth := 1; depth <= 32; depth++ {
			entryID := uint64(1000 + depth)
			name := fmt.Sprintf("depth-%d", depth)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,?,?,?,'DIR')", entryID, parentID, name, []byte(name)); err != nil {
				t.Fatal(err)
			}
			parentID = entryID
		}
		if result, err := lists.List(ctx, actor, 1031, nil, 2); err != nil || result == nil || len(result.Path.Entries) != 32 || len(result.Entries) != 1 || result.Entries[0].ID != 1032 {
			t.Fatal("合法第 32 层子项目录失败", err)
		}
		if result, err := lists.List(ctx, actor, 1032, nil, 2); err != nil || result == nil || len(result.Entries) != 0 {
			t.Fatal("第 32 层空目录失败", err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(1033,1,1032,'超深子项',CAST('超深子项' AS BINARY),'DIR')"); err != nil {
			t.Fatal(err)
		}
		if result, err := lists.List(ctx, actor, 1032, nil, 2); result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
			t.Fatal("第 33 层子项未拒绝", err)
		}
	})
	t.Run("full unsigned list fields", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','最大用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','fixture-directory-list-max',?)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum-1, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(?,?,?,'最大文件',CAST('最大文件' AS BINARY),'FILE','OTHER',?,?,?)", maximum, maximum, maximum-1, maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		identity := domain.Actor{UserID: maximum, SessionID: maximum, DeviceID: maximum}
		result, err := lists.List(ctx, identity, maximum-1, nil, 1)
		if err != nil || result == nil || len(result.Path.Entries) != 1 || result.Path.Entries[0].ID != maximum-1 || len(result.Entries) != 1 || result.HasMore || result.Entries[0].ID != maximum || result.Entries[0].FileSize != maximum || result.Entries[0].Revision != maximum {
			t.Fatal("完整无符号列表字段错误", err)
		}
		last := result.Entries[0]
		if next, err := lists.List(ctx, identity, maximum-1, &domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID}, 1); err != nil || next == nil || len(next.Entries) != 0 {
			t.Fatal("最大标识的末页位置错误", err)
		}
	})
}
