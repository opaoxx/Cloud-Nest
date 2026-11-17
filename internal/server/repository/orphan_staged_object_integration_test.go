package repository

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestMarkUserStagedObjectOrphanedIntegration(t *testing.T) {
	// 1. 复用正式对象、目录和任务表，补齐当前事务身份。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表定义未找到")
	}
	for _, statement := range []string{
		migrations.InitialSQL[start:end],
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(25,7,'MINIO','fixture-bucket','other-attempt',16777216)",
		"UPDATE stored_objects SET storage_version_id='fixture-version',storage_etag='fixture-etag',last_error_code='STORAGE_RETRY' WHERE id=23",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	mark := func(current domain.Actor, objectID uint64) (*domain.StoredObject, error) {
		var object *domain.StoredObject
		err := store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
			var err error
			object, err = txStore.MarkUserStagedObjectOrphaned(txContext, objectID)
			return err
		})
		if err != nil {
			return nil, err
		}
		return object, nil
	}
	reset := func(t *testing.T) {
		t.Helper()
		for _, statement := range []string{
			"UPDATE stored_objects SET status='STAGING',unreferenced_at=NULL,gc_after=NULL WHERE id=23",
			"UPDATE upload_tasks SET staged_object_id=23,published_object_id=29,status='COMPLETED' WHERE id=19",
			"UPDATE file_entries SET object_id=29,status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL WHERE id=31",
		} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Run("single connection grace and stable replay", func(t *testing.T) {
		// 2. 孤立状态和七天期限落库，重试保留全部时间与对象事实。
		reset(t)
		before, err := store.FindUserStoredObject(ctx, 7, 23)
		if err != nil {
			t.Fatal(err)
		}
		profile, err := store.FindUserProfile(ctx, 7)
		if err != nil {
			t.Fatal(err)
		}
		var earliest, latest time.Time
		if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&earliest); err != nil {
			t.Fatal(err)
		}
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		object, err := mark(actor, 23)
		if err != nil {
			t.Fatal("单连接孤立标记失败", err)
		}
		if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&latest); err != nil {
			t.Fatal(err)
		}
		if object.Status != domain.StoredObjectStatusOrphaned || object.UnreferencedAt == nil || object.GCAfter == nil ||
			!object.UnreferencedAt.Equal(object.UpdatedAt) || !object.GCAfter.Equal(object.UnreferencedAt.Add(7*24*time.Hour)) ||
			object.UpdatedAt.Before(earliest) || object.UpdatedAt.After(latest) || object.UpdatedAt.Nanosecond()%1000 != 0 {
			t.Fatal("孤立对象数据库时间或七天期限错误")
		}
		expected := *before
		expected.Status, expected.UnreferencedAt, expected.GCAfter, expected.UpdatedAt = object.Status, object.UnreferencedAt, object.GCAfter, object.UpdatedAt
		if !reflect.DeepEqual(object, &expected) {
			t.Fatal("孤立标记改变了额外对象事实")
		}
		again, err := mark(actor, 23)
		if err != nil || !reflect.DeepEqual(again, object) {
			t.Fatal("重复标记重置了连续无引用起点", err)
		}
		afterProfile, err := store.FindUserProfile(ctx, 7)
		if err != nil || !reflect.DeepEqual(profile, afterProfile) {
			t.Fatal("孤立标记改变了用户空间事实", err)
		}
	})
	for _, status := range []string{"ACTIVE", "TRASHED", "PURGING"} {
		t.Run("file reference "+status, func(t *testing.T) {
			reset(t)
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET object_id=23,status=?,deleted_at=CASE WHEN ?='TRASHED' THEN UTC_TIMESTAMP(6) ELSE NULL END,purge_started_at=CASE WHEN ?='PURGING' THEN UTC_TIMESTAMP(6) ELSE NULL END WHERE id=31", status, status, status); err != nil {
				t.Fatal(err)
			}
			if object, err := mark(actor, 23); object != nil || !errors.Is(err, domain.ErrStoredObjectInUse) {
				t.Fatal("有效文件引用未阻止孤立标记", err)
			}
		})
	}
	for _, status := range []domain.UploadStatus{
		domain.UploadStatusCreating, domain.UploadStatusUploading, domain.UploadStatusCompleting,
		domain.UploadStatusVerifying, domain.UploadStatusCompleted, domain.UploadStatusCancelling,
		domain.UploadStatusCancelled, domain.UploadStatusExpiring, domain.UploadStatusExpired, domain.UploadStatusFailed,
	} {
		t.Run("task reference "+string(status), func(t *testing.T) {
			reset(t)
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status=? WHERE id=19", string(status)); err != nil {
				t.Fatal(err)
			}
			object, err := mark(actor, 23)
			if status.IsTerminal() {
				if err != nil || object.Status != domain.StoredObjectStatusOrphaned {
					t.Fatal("终态审计引用阻止了旧暂存对象孤立", err)
				}
			} else if object != nil || !errors.Is(err, domain.ErrStoredObjectInUse) {
				t.Fatal("未终结任务引用未阻止孤立标记", err)
			}
		})
	}
	t.Run("published reference in unfinished task", func(t *testing.T) {
		reset(t)
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET staged_object_id=25,published_object_id=23,status='VERIFYING' WHERE id=19"); err != nil {
			t.Fatal(err)
		}
		if object, err := mark(actor, 23); object != nil || !errors.Is(err, domain.ErrStoredObjectInUse) {
			t.Fatal("未终结任务发布引用未阻止孤立标记", err)
		}
	})
	t.Run("purged entry releases content", func(t *testing.T) {
		reset(t)
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='PURGED',object_id=NULL,purged_at=UTC_TIMESTAMP(6) WHERE id=31"); err != nil {
			t.Fatal(err)
		}
		if _, err := mark(actor, 23); err != nil {
			t.Fatal("已释放内容的墓碑阻止了孤立标记", err)
		}
	})
	t.Run("current reference after old snapshot", func(t *testing.T) {
		// 3. 旧快照形成后文件引用变化，当前读仍须识别有效引用。
		reset(t)
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if _, err := txStore.FindUserFileEntry(txContext, 7, 31); err != nil {
				return err
			}
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET object_id=23 WHERE id=31"); err != nil {
				return err
			}
			_, err := txStore.MarkUserStagedObjectOrphaned(txContext, 23)
			return err
		})
		if !errors.Is(err, domain.ErrStoredObjectInUse) {
			t.Fatal("已有快照掩盖了当前有效引用", err)
		}
	})
	t.Run("scope and unsupported stage", func(t *testing.T) {
		for _, fixture := range []struct {
			objectID  uint64
			wantError error
		}{{24, domain.ErrStoredObjectNotFound}, {999, domain.ErrStoredObjectNotFound}, {29, domain.ErrInvalidStoredObjectData}} {
			if object, err := mark(actor, fixture.objectID); object != nil || !errors.Is(err, fixture.wantError) {
				t.Fatal("对象归属或阶段边界错误", err)
			}
		}
	})
	for _, mode := range []string{"callback failure", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			reset(t)
			before, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("outer orphan failure")
			cancelled, stop := context.WithCancel(ctx)
			err = store.WithUserTransaction(cancelled, actor, func(txContext context.Context, txStore *Store) error {
				if _, err := txStore.MarkUserStagedObjectOrphaned(txContext, 23); err != nil {
					return err
				}
				if mode == "cancelled" {
					stop()
					return nil
				}
				return failure
			})
			stop()
			wantError := failure
			if mode == "cancelled" {
				wantError = context.Canceled
			}
			if !errors.Is(err, wantError) {
				t.Fatal("孤立标记外层失败原因错误", err)
			}
			after, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("孤立标记外层失败未完整回滚", err)
			}
		})
	}
	for _, mode := range []string{"commit", "rollback"} {
		t.Run("task replacement and orphan "+mode, func(t *testing.T) {
			reset(t)
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status='UPLOADING' WHERE id=19"); err != nil {
				t.Fatal(err)
			}
			beforeTask, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			beforeObject, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("task replacement rollback")
			err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				// 用测试 SQL 模拟下一小步的引用切换，验证同事务读取自己的写入。
				if err := txStore.database.WithContext(txContext).Exec("UPDATE upload_tasks SET staged_object_id=25 WHERE user_id=7 AND id=19").Error; err != nil {
					return err
				}
				if _, err := txStore.MarkUserStagedObjectOrphaned(txContext, 23); err != nil {
					return err
				}
				if mode == "rollback" {
					return failure
				}
				return nil
			})
			afterTask, readError := store.FindUserUploadTask(ctx, 7, 19)
			if readError != nil {
				t.Fatal(readError)
			}
			afterObject, readError := store.FindUserStoredObject(ctx, 7, 23)
			if readError != nil {
				t.Fatal(readError)
			}
			if mode == "rollback" {
				if !errors.Is(err, failure) || !reflect.DeepEqual(beforeTask, afterTask) || !reflect.DeepEqual(beforeObject, afterObject) {
					t.Fatal("任务引用与孤立状态未共同回滚", err)
				}
			} else if err != nil || afterTask.StagedObjectID != 25 || afterObject.Status != domain.StoredObjectStatusOrphaned {
				t.Fatal("任务引用与孤立状态未共同提交", err)
			}
		})
	}
	t.Run("concurrent replay retains original grace", func(t *testing.T) {
		reset(t)
		type result struct {
			object *domain.StoredObject
			err    error
		}
		var workers sync.WaitGroup
		start := make(chan struct{})
		results := make(chan result, 2)
		for range 2 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				object, err := mark(actor, 23)
				results <- result{object, err}
			}()
		}
		close(start)
		workers.Wait()
		first, second := <-results, <-results
		if first.err != nil || second.err != nil || !reflect.DeepEqual(first.object, second.object) {
			t.Fatal("并发重复标记改变了宽限期", first.err, second.err)
		}
	})
	t.Run("maximum unsigned identities and size", func(t *testing.T) {
		maximum := ^uint64(0)
		for _, fixture := range []struct {
			statement string
			arguments []any
		}{
			{"INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'orphan_object_max','完整用户','fixture_hash')", []any{maximum}},
			{"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'b8ee758a-0727-42de-ae7d-3bb33155e148','完整设备','WEB')", []any{maximum, maximum}},
			{"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", []any{maximum, maximum, maximum}},
			{"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','orphan-object-max',?)", []any{maximum, maximum, maximum}},
		} {
			if _, err := pool.ExecContext(ctx, fixture.statement, fixture.arguments...); err != nil {
				t.Fatal(err)
			}
		}
		object, err := mark(domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}, maximum)
		if err != nil || object.ID != maximum || object.OwnerUserID != maximum || object.FileSize != maximum || object.Status != domain.StoredObjectStatusOrphaned {
			t.Fatal("完整无符号孤立标记失败", err)
		}
	})
}
