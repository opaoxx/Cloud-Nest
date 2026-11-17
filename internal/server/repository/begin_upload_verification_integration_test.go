package repository

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestBeginUserUploadVerificationIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表定义未找到")
	}
	for _, statement := range []string{
		migrations.InitialSQL[start:end],
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(15,7,'c8ee758a-0727-42de-ae7d-3bb33155e148','其他设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),(18,7,15,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
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
	reset := func(t *testing.T) {
		t.Helper()
		for _, statement := range []string{
			"UPDATE upload_tasks SET staged_object_id=23,status='UPLOADING',upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,file_size=16777216,reserved_bytes=16777216,lease_owner='current-worker',lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,lease_version=3,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR,attempt_count=4,last_error_code='STORAGE_UNAVAILABLE',last_error_message='old diagnostic' WHERE id=19",
			"UPDATE stored_objects SET status='STAGING',file_size=16777216,storage_version_id=NULL,storage_etag=NULL,verified_sha256=NULL,verified_at=NULL,unreferenced_at=NULL,gc_after=NULL,last_error_code='STORAGE_RETRY' WHERE id=23",
		} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	etag, version := "opaque-etag", "null"
	confirmation := domain.UploadObjectResult{SizeBytes: domain.MaxSingleUploadSizeBytes, StorageETag: &etag, StorageVersionID: &version}
	advance := func(current domain.Actor, taskID, objectID, leaseVersion uint64, input domain.UploadObjectResult) (*domain.UploadTask, error) {
		var result *domain.UploadTask
		err := store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
			var err error
			result, err = txStore.BeginUserUploadVerification(txContext, taskID, objectID, leaseVersion, "current-worker", input)
			return err
		})
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	read := func(t *testing.T) (*domain.UploadTask, *domain.StoredObject) {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		object, err := store.FindUserStoredObject(ctx, 7, 23)
		if err != nil {
			t.Fatal(err)
		}
		return task, object
	}
	for _, mode := range []string{"ordinary", "zero bytes", "nil metadata", "other device"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 单连接提交两表状态和确认信息，完整核对允许变化的字段。
			reset(t)
			input, current := confirmation, actor
			if mode == "zero bytes" {
				for _, statement := range []string{"UPDATE upload_tasks SET file_size=0,reserved_bytes=0 WHERE id=19", "UPDATE stored_objects SET file_size=0 WHERE id=23"} {
					if _, err := pool.ExecContext(ctx, statement); err != nil {
						t.Fatal(err)
					}
				}
				input.SizeBytes = 0
			}
			if mode == "nil metadata" {
				input.StorageETag, input.StorageVersionID = nil, nil
			}
			if mode == "other device" {
				current = domain.Actor{UserID: 7, DeviceID: 15, SessionID: 18}
			}
			beforeTask, beforeObject := read(t)
			var quotaBefore, quotaAfter [3]uint64
			if err := pool.QueryRowContext(ctx, "SELECT storage_quota,used_storage,reserved_storage FROM users WHERE id=7").Scan(&quotaBefore[0], &quotaBefore[1], &quotaBefore[2]); err != nil {
				t.Fatal(err)
			}
			result, err := advance(current, 19, 23, 3, input)
			if err != nil || result == nil {
				t.Fatal("进入校验阶段失败", err)
			}
			afterTask, afterObject := read(t)
			expectedTask := *beforeTask
			expectedTask.Status = domain.UploadStatusVerifying
			expectedTask.LeaseOwner, expectedTask.LeaseUntil, expectedTask.NextAttemptAt = nil, nil, nil
			expectedTask.LastErrorCode, expectedTask.LastErrorMessage = nil, nil
			expectedTask.UpdatedAt = result.UpdatedAt
			expectedObject := *beforeObject
			expectedObject.Status, expectedObject.LastErrorCode = domain.StoredObjectStatusVerifying, nil
			expectedObject.StorageETag, expectedObject.StorageVersionID = input.StorageETag, input.StorageVersionID
			expectedObject.UpdatedAt = result.UpdatedAt
			if !reflect.DeepEqual(result, &expectedTask) || !reflect.DeepEqual(afterTask, &expectedTask) || !reflect.DeepEqual(afterObject, &expectedObject) {
				t.Fatal("两表状态、元数据、租约或完整事实错误")
			}
			if err := pool.QueryRowContext(ctx, "SELECT storage_quota,used_storage,reserved_storage FROM users WHERE id=7").Scan(&quotaAfter[0], &quotaAfter[1], &quotaAfter[2]); err != nil || quotaBefore != quotaAfter {
				t.Fatal("进入校验改变了用户配额", err)
			}
			// 重复推进按当前阶段返回，数据库结果继续表示已进入校验。
			if repeated, err := advance(current, 19, 23, 3, input); !errors.Is(err, domain.ErrUploadTaskNotReady) || repeated != nil {
				t.Fatal("重复推进的阶段判断错误", err)
			}
			repeatedTask, repeatedObject := read(t)
			if !reflect.DeepEqual(afterTask, repeatedTask) || !reflect.DeepEqual(afterObject, repeatedObject) {
				t.Fatal("重复推进改写了校验事实")
			}
		})
	}
	for _, mode := range []string{"old lease", "expired lease", "wrong object", "wrong object stage", "conflicting metadata"} {
		t.Run(mode, func(t *testing.T) {
			reset(t)
			objectID, leaseVersion := uint64(23), uint64(3)
			want := domain.ErrUploadTaskBusy
			statement := ""
			switch mode {
			case "old lease":
				leaseVersion = 2
			case "expired lease":
				statement = "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19"
			case "wrong object":
				objectID = 24
			case "wrong object stage":
				statement, want = "UPDATE stored_objects SET status='VERIFYING' WHERE id=23", domain.ErrInvalidStoredObjectData
			case "conflicting metadata":
				statement, want = "UPDATE stored_objects SET storage_etag='conflicting' WHERE id=23", domain.ErrInvalidStoredObjectData
			}
			if statement != "" {
				if _, err := pool.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			beforeTask, beforeObject := read(t)
			result, err := advance(actor, 19, objectID, leaseVersion, confirmation)
			afterTask, afterObject := read(t)
			if !errors.Is(err, want) || result != nil || !reflect.DeepEqual(beforeTask, afterTask) || !reflect.DeepEqual(beforeObject, afterObject) {
				t.Fatal("拒绝推进后出现部分变化", err)
			}
		})
	}
	for _, mode := range []string{"transition SQL failure", "outer rollback", "cancel after transition"} {
		t.Run(mode, func(t *testing.T) {
			reset(t)
			beforeTask, beforeObject := read(t)
			if mode == "transition SQL failure" {
				if _, err := pool.ExecContext(ctx, "CREATE TRIGGER fail_verify_transition BEFORE UPDATE ON upload_tasks FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture verification fault'"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cleanupContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					if _, err := pool.ExecContext(cleanupContext, "DROP TRIGGER fail_verify_transition"); err != nil {
						t.Error(err)
					}
				})
			}
			current, stop := context.WithCancel(ctx)
			defer stop()
			fault := errors.New("after verification transition")
			err := store.WithUserTransaction(current, actor, func(txContext context.Context, txStore *Store) error {
				if _, err := txStore.BeginUserUploadVerification(txContext, 19, 23, 3, "current-worker", confirmation); err != nil {
					return err
				}
				if mode == "cancel after transition" {
					stop()
					return nil
				}
				return fault
			})
			afterTask, afterObject := read(t)
			if mode == "transition SQL failure" {
				var databaseError *mysql.MySQLError
				if !errors.As(err, &databaseError) || databaseError.Number != 1644 {
					t.Fatal("未取得触发器注入的状态 SQL 错误", err)
				}
			} else {
				want := fault
				if mode == "cancel after transition" {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatal("外层失败原因错误", err)
				}
			}
			if !reflect.DeepEqual(beforeTask, afterTask) || !reflect.DeepEqual(beforeObject, afterObject) {
				t.Fatal("确认信息和两表状态未整体回滚")
			}
		})
	}
	t.Run("concurrent one transition", func(t *testing.T) {
		reset(t)
		pool.SetMaxOpenConns(3)
		var callers sync.WaitGroup
		proceed := make(chan struct{})
		results := make(chan error, 2)
		for range 2 {
			callers.Go(func() {
				<-proceed
				_, err := advance(actor, 19, 23, 3, confirmation)
				results <- err
			})
		}
		close(proceed)
		callers.Wait()
		close(results)
		successes, notReady := 0, 0
		for err := range results {
			if err == nil {
				successes++
			} else if errors.Is(err, domain.ErrUploadTaskNotReady) {
				notReady++
			} else {
				t.Fatal(err)
			}
		}
		task, object := read(t)
		if successes != 1 || notReady != 1 || task.Status != domain.UploadStatusVerifying || object.Status != domain.StoredObjectStatusVerifying || task.LeaseOwner != nil || task.LeaseUntil != nil {
			t.Fatal("并发状态推进结果错误", successes, notReady)
		}
	})
	t.Run("maximum unsigned identities and lease version", func(t *testing.T) {
		maximum := ^uint64(0)
		for _, fixture := range []struct {
			statement string
			arguments []any
		}{
			{"INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'begin_verify_max','完整用户','fixture_hash')", []any{maximum}},
			{"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'f8ee758a-0727-42de-ae7d-3bb33155e148','完整设备','WEB')", []any{maximum, maximum}},
			{"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", []any{maximum, maximum, maximum}},
			{"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','verify-max',0)", []any{maximum, maximum}},
			{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", []any{maximum, maximum}},
			{"INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,lease_owner,lease_until,lease_version,expires_at) VALUES(?,?,?,?,?,'maximum.bin','SINGLE',0,'UPLOADING',0,'current-worker',UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,?,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)", []any{maximum, maximum, maximum, maximum, maximum, maximum}},
		} {
			if _, err := pool.ExecContext(ctx, fixture.statement, fixture.arguments...); err != nil {
				t.Fatal(err)
			}
		}
		input := domain.UploadObjectResult{StorageETag: &etag, StorageVersionID: &version}
		result, err := advance(domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}, maximum, maximum, maximum, input)
		if err != nil || result == nil || result.ID != maximum || result.StagedObjectID != maximum || result.LeaseVersion != maximum || result.Status != domain.UploadStatusVerifying || result.LeaseOwner != nil {
			t.Fatal("完整无符号校验阶段推进失败", err)
		}
		object, err := store.FindUserStoredObject(ctx, maximum, maximum)
		if err != nil || object.Status != domain.StoredObjectStatusVerifying {
			t.Fatal("完整无符号对象状态错误", err)
		}
	})
}
