package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

// entryDetailChangingReader 核对读取顺序，并在基础记录读取后触发受控并发变更。
type entryDetailChangingReader struct {
	*Store
	afterRead                     func(uint64) error
	displayCalls, preferenceCalls int
	displayError, preferenceError error
}

func (reader *entryDetailChangingReader) FindUserFileEntry(ctx context.Context, userID, entryID uint64) (*domain.FileEntry, error) {
	entry, err := reader.Store.FindUserFileEntry(ctx, userID, entryID)
	if err == nil && reader.afterRead != nil {
		err = reader.afterRead(entryID)
	}
	return entry, err
}

func (reader *entryDetailChangingReader) FindUserFileEntryDisplay(ctx context.Context, userID, entryID uint64) (*domain.FileEntryDisplay, error) {
	reader.displayCalls++
	if reader.displayError != nil {
		return nil, reader.displayError
	}
	return reader.Store.FindUserFileEntryDisplay(ctx, userID, entryID)
}

func (reader *entryDetailChangingReader) FindUserEntryPreference(ctx context.Context, userID, entryID uint64) (*domain.EntryPreference, error) {
	reader.preferenceCalls++
	if reader.preferenceError != nil {
		return nil, reader.preferenceError
	}
	return reader.Store.FindUserEntryPreference(ctx, userID, entryID)
}

func TestEntryDetailServiceIntegration(t *testing.T) {
	// 1. 从固定迁移建立正式目录及偏好表，装配单次只读快照服务。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE user_entry_preferences (")
	if start < 0 {
		t.Fatal("未找到正式偏好表")
	}
	end := strings.Index(migrations.InitialSQL[start:], "CREATE TABLE upload_tasks (")
	if end < 0 {
		t.Fatal("未找到偏好表结束边界")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:start+end]); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','详情用户','fixture_hash','ACTIVE'),(2,'fixture_two','禁用用户','fixture_hash','DISABLED'),(3,'fixture_three','其他用户','fixture_hash','ACTIVE'),(4,'fixture_four','缺根用户','fixture_hash','ACTIVE')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(19,1,'28ee758a-0727-42de-ae7d-3bb33155e148','设备当前名称','WEB')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-detail-service',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,color_key) VALUES(11,1,NULL,'',X'','DIR',NULL),(22,2,NULL,'',X'','DIR',NULL),(33,3,NULL,'',X'','DIR',NULL),(14,1,11,'祖先目录',CAST('祖先目录' AS BINARY),'DIR',NULL),(15,1,11,'其他目录',CAST('其他目录' AS BINARY),'DIR',NULL),(12,1,14,'课程资料',CAST('课程资料' AS BINARY),'DIR','green')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,mime_type,object_id,file_size,revision,source_device_id,source_device_name,content_modified_at) VALUES(13,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT','md','text/markdown',41,2048,3,19,'发布时设备 🎮','2020-01-02 03:04:05.123456')",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,13,'2020-01-02 03:04:05.123456','2020-01-02 03:04:06.654321'),(1,12,'2020-01-02 03:04:05.123456',NULL)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立详情服务夹具失败", err)
		}
	}
	stamp := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	details := service.NewEntryDetailService(func(ctx context.Context, action func(context.Context, service.EntryDetailReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
			return action(snapshotContext, snapshotStore)
		})
	})
	actor := domain.Actor{UserID: 1, SessionID: 17, DeviceID: 19}
	checkPath := func(t *testing.T, result *domain.EntryDetail, pathIDs []uint64) {
		t.Helper()
		if result == nil || len(result.Path.Entries) != len(pathIDs) {
			t.Fatal("详情路径形状错误")
		}
		for index, entry := range result.Path.Entries {
			if entry.ID != pathIDs[index] || entry.UserID != actor.UserID || entry.Status != "ACTIVE" {
				t.Fatal("详情路径顺序、归属或状态错误")
			}
		}
		last := pathIDs[len(pathIDs)-1]
		if result.Display.ID != last || result.Display.UserID != actor.UserID || result.Preference.EntryID != last || result.Preference.UserID != actor.UserID {
			t.Fatal("详情各部分目标或用户不一致")
		}
	}

	// 2. 验证文件、目录、根目录、默认偏好及独立结果。
	t.Run("file directory root and independent results", func(t *testing.T) {
		first, err := details.Get(ctx, actor, 13)
		if err != nil {
			t.Fatal(err)
		}
		checkPath(t, first, []uint64{11, 14, 12, 13})
		if first.Path.Entries[3].FileSize != 2048 || first.Path.Entries[3].Revision != 3 ||
			first.Display.Category == nil || *first.Display.Category != "DOCUMENT" || *first.Display.Extension != "md" ||
			*first.Display.MIMEType != "text/markdown" || *first.Display.SourceDeviceID != 19 || *first.Display.SourceDeviceName != "发布时设备 🎮" ||
			!first.Display.ContentModifiedAt.Equal(stamp) || !first.Preference.StarredAt.Equal(stamp) || first.Preference.LastOpenedAt == nil {
			t.Fatal("文件基础、展示或偏好字段错误")
		}
		second, err := details.Get(ctx, actor, 13)
		if err != nil || !reflect.DeepEqual(first, second) {
			t.Fatal(err)
		}
		first.Path.Entries[3].Name, *first.Path.Entries[3].ParentID, *first.Display.Extension, *first.Preference.StarredAt = "本地修改", 99, "changed", time.Time{}
		again, err := details.Get(ctx, actor, 13)
		if err != nil || !reflect.DeepEqual(second, again) {
			t.Fatal("修改详情结果影响了另一次读取", err)
		}
		directory, err := details.Get(ctx, actor, 12)
		if err != nil {
			t.Fatal(err)
		}
		checkPath(t, directory, []uint64{11, 14, 12})
		if directory.Display.Category != nil || directory.Display.ColorKey == nil || *directory.Display.ColorKey != "green" || directory.Preference.LastOpenedAt != nil {
			t.Fatal("目录颜色、类型或可空偏好错误")
		}
		root, err := details.Get(ctx, actor, 11)
		if err != nil {
			t.Fatal(err)
		}
		checkPath(t, root, []uint64{11})
		if root.Path.Entries[0].Name != "" || root.Preference.StarredAt != nil || root.Preference.LastOpenedAt != nil || root.Display.ColorKey != nil {
			t.Fatal("根目录名称、展示空值或默认偏好错误")
		}
	})
	t.Run("target and ancestors must remain visible", func(t *testing.T) {
		for _, targetID := range []uint64{13, 12, 14} {
			for _, state := range []string{"TRASHED", "PURGING", "PURGED"} {
				t.Run(fmt.Sprintf("%d/%s", targetID, state), func(t *testing.T) {
					if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,object_id=IF(entry_type='FILE' AND ?<>'PURGED',41,NULL),deleted_at=UTC_TIMESTAMP(6),purge_started_at=UTC_TIMESTAMP(6),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL) WHERE id=?", state, state, state, targetID); err != nil {
						t.Fatal(err)
					}
					defer func() {
						if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',object_id=IF(entry_type='FILE',41,NULL),deleted_at=NULL,purge_started_at=NULL,purged_at=NULL WHERE id=?", targetID); err != nil {
							t.Error(err)
						}
					}()
					var reader *entryDetailChangingReader
					checked := service.NewEntryDetailService(func(ctx context.Context, action func(context.Context, service.EntryDetailReader) error) error {
						return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
							reader = &entryDetailChangingReader{Store: snapshotStore}
							return action(snapshotContext, reader)
						})
					})
					result, err := checked.Get(ctx, actor, 13)
					if result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) || reader == nil || reader.displayCalls != 0 || reader.preferenceCalls != 0 {
						t.Fatal("不可见路径仍读取了展示或偏好", err)
					}
				})
			}
		}
	})
	t.Run("account and target isolation", func(t *testing.T) {
		for _, testCase := range []struct {
			userID, entryID uint64
			want            error
		}{
			{1, 33, domain.ErrFileEntryNotFound}, {3, 13, domain.ErrFileEntryNotFound}, {1, 999, domain.ErrFileEntryNotFound},
			{2, 22, domain.ErrAccountDisabled}, {4, 13, domain.ErrInvalidAccountData}, {99, 13, domain.ErrAccountNotFound},
		} {
			identity := actor
			identity.UserID = testCase.userID
			if result, err := details.Get(ctx, identity, testCase.entryID); result != nil || !errors.Is(err, testCase.want) {
				t.Fatal("详情账号状态或目标归属错误", err)
			}
		}
	})

	// 3. 在读取目标之后提交移动、回收、展示和偏好变更，所有详情仍来自同一快照。
	t.Run("path display and preference share one snapshot", func(t *testing.T) {
		original, err := details.Get(ctx, actor, 13)
		if err != nil {
			t.Fatal(err)
		}
		snapshots := 0
		changing := service.NewEntryDetailService(func(ctx context.Context, action func(context.Context, service.EntryDetailReader) error) error {
			snapshots++
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
				reader := &entryDetailChangingReader{Store: snapshotStore, afterRead: func(entryID uint64) error {
					if entryID != 13 {
						return nil
					}
					writer, err := pool.BeginTx(ctx, nil)
					if err != nil {
						return err
					}
					defer writer.Rollback()
					for _, statement := range []string{
						"UPDATE file_entries SET parent_id=15,color_key='sand',revision=revision+1 WHERE id=12",
						"UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=14",
						"UPDATE file_entries SET name='更新笔记.txt',name_key=CAST('更新笔记.txt' AS BINARY),extension='txt',mime_type='text/plain',source_device_name='新的发布名称',revision=revision+1 WHERE id=13",
						"UPDATE user_entry_preferences SET starred_at=NULL,last_opened_at=DATE_ADD(last_opened_at,INTERVAL 1 SECOND) WHERE user_id=1 AND entry_id=13",
					} {
						if _, err := writer.ExecContext(ctx, statement); err != nil {
							return err
						}
					}
					return writer.Commit()
				}}
				return action(snapshotContext, reader)
			})
		})
		consistent, err := changing.Get(ctx, actor, 13)
		if err != nil || snapshots != 1 || !reflect.DeepEqual(consistent, original) {
			t.Fatal("详情组合未使用单次一致快照", err)
		}
		current, err := details.Get(ctx, actor, 13)
		if err != nil {
			t.Fatal(err)
		}
		checkPath(t, current, []uint64{11, 15, 12, 13})
		if current.Path.Entries[3].Name != "更新笔记.txt" || current.Path.Entries[3].Revision != 4 || *current.Display.Extension != "txt" ||
			*current.Display.SourceDeviceName != "新的发布名称" || current.Preference.StarredAt != nil || !current.Preference.LastOpenedAt.Equal(original.Preference.LastOpenedAt.Add(time.Second)) {
			t.Fatal("新快照未读到已提交的详情变更")
		}
	})
	t.Run("query and snapshot failures discard result", func(t *testing.T) {
		for _, mode := range []string{"display failure", "preference failure", "end failure", "cancel"} {
			t.Run(mode, func(t *testing.T) {
				requestContext, stop := context.WithCancel(ctx)
				defer stop()
				failure := errors.New("fixture detail snapshot")
				failing := service.NewEntryDetailService(func(ctx context.Context, action func(context.Context, service.EntryDetailReader) error) error {
					return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
						reader := &entryDetailChangingReader{Store: snapshotStore}
						if mode == "display failure" {
							reader.displayError = failure
						}
						if mode == "preference failure" {
							reader.preferenceError = failure
						}
						if err := action(snapshotContext, reader); err != nil {
							return err
						}
						if mode == "cancel" {
							stop()
							return nil
						}
						return failure
					})
				})
				want := error(failure)
				if mode == "cancel" {
					want = context.Canceled
				}
				if result, err := failing.Get(requestContext, actor, 13); result != nil || !errors.Is(err, want) {
					t.Fatal("详情故障或取消未丢弃结果", err)
				}
				if result, err := details.Get(ctx, actor, 13); err != nil || result == nil {
					t.Fatal("故障后正常详情读取未恢复", err)
				}
			})
		}
	})
	// 4. 最大主键夹具最后写入，验证完整标识、版本、大小及可空时间边界。
	t.Run("maximum identifiers and database time range", func(t *testing.T) {
		maximum := ^uint64(0)
		minimumDate := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
		maximumDate := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','完整整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'edbf7e35-36ea-45a5-bdb9-58e76b46fd5e','完整整数设备','ANDROID')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','fixture-detail-max',?)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision,source_device_id,source_device_name,content_modified_at) VALUES(?,?,?,'最大文件.bin',CAST('最大文件.bin' AS BINARY),'FILE','OTHER',?,?,?,?,'完整整数设备',?)", maximum-1, maximum, maximum, maximum, maximum, maximum, maximum, minimumDate); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(?,?,?,?)", maximum, maximum-1, minimumDate, maximumDate); err != nil {
			t.Fatal(err)
		}
		identity := domain.Actor{UserID: maximum, SessionID: maximum, DeviceID: maximum}
		result, err := details.Get(ctx, identity, maximum-1)
		if err != nil || result == nil || len(result.Path.Entries) != 2 || result.Path.Entries[1].FileSize != maximum || result.Path.Entries[1].Revision != maximum ||
			result.Display.ID != maximum-1 || result.Display.UserID != maximum || result.Display.SourceDeviceID == nil || *result.Display.SourceDeviceID != maximum ||
			!result.Display.ContentModifiedAt.Equal(minimumDate) || !result.Preference.StarredAt.Equal(minimumDate) || !result.Preference.LastOpenedAt.Equal(maximumDate) {
			t.Fatal("详情完整整数或数据库日期边界错误", err)
		}
	})
}
