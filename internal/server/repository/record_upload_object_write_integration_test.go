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

func TestRecordUserUploadObjectWriteIntegration(t *testing.T) {
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
		"INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at) VALUES(21,8,12,24,14,'foreign.bin','SINGLE',0,'UPLOADING',0,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)",
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
			"UPDATE users SET status='ACTIVE' WHERE id=7",
			"UPDATE user_devices SET revoked_at=NULL WHERE id=11",
			"UPDATE auth_sessions SET revoked_at=NULL,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=17",
			"UPDATE upload_tasks SET staged_object_id=23,status='UPLOADING',upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,file_size=16777216,reserved_bytes=16777216,lease_owner='current-worker',lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,lease_version=3,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=19",
			"UPDATE stored_objects SET status='STAGING',file_size=16777216,storage_version_id=NULL,storage_etag=NULL,verified_sha256=NULL,verified_at=NULL,unreferenced_at=NULL,gc_after=NULL,last_error_code=NULL WHERE id=23",
		} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	record := func(current domain.Actor, taskID, objectID, version uint64, owner string, confirmation domain.UploadObjectResult) (*domain.StoredObject, error) {
		var result *domain.StoredObject
		err := store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
			var err error
			result, err = txStore.RecordUserUploadObjectWrite(txContext, taskID, objectID, version, owner, confirmation)
			return err
		})
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	for _, mode := range []string{"ordinary", "zero bytes", "nil metadata", "empty metadata", "column boundary", "other device", "repeat", "fill missing", "preserve known"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 只更新存储确认字段，逐项核对完整任务和配额事实。
			reset(t)
			etag, version := "opaque-etag", "null"
			confirmation := domain.UploadObjectResult{SizeBytes: domain.MaxSingleUploadSizeBytes, StorageETag: &etag, StorageVersionID: &version}
			current := actor
			switch mode {
			case "zero bytes":
				for _, statement := range []string{"UPDATE upload_tasks SET file_size=0,reserved_bytes=0 WHERE id=19", "UPDATE stored_objects SET file_size=0 WHERE id=23"} {
					if _, err := pool.ExecContext(ctx, statement); err != nil {
						t.Fatal(err)
					}
				}
				confirmation.SizeBytes = 0
			case "nil metadata":
				confirmation.StorageETag, confirmation.StorageVersionID = nil, nil
			case "empty metadata":
				etag, version = "", ""
			case "column boundary":
				etag, version = strings.Repeat("e", 255), strings.Repeat("v", 255)
			case "other device":
				current = domain.Actor{UserID: 7, DeviceID: 15, SessionID: 18}
			case "fill missing", "preserve known":
				if _, err := pool.ExecContext(ctx, "UPDATE stored_objects SET storage_etag=? WHERE id=23", etag); err != nil {
					t.Fatal(err)
				}
				confirmation.StorageETag = nil
				if mode == "preserve known" {
					confirmation.StorageVersionID = nil
				}
			}
			beforeTask, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			var quotaBefore, quotaAfter [3]uint64
			if err := pool.QueryRowContext(ctx, "SELECT storage_quota,used_storage,reserved_storage FROM users WHERE id=7").Scan(&quotaBefore[0], &quotaBefore[1], &quotaBefore[2]); err != nil {
				t.Fatal(err)
			}
			result, err := record(current, 19, 23, 3, "current-worker", confirmation)
			stored, readError := store.FindUserStoredObject(ctx, 7, 23)
			afterTask, taskError := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil || readError != nil || taskError != nil || result == nil || !reflect.DeepEqual(result, stored) || !reflect.DeepEqual(beforeTask, afterTask) || stored.Status != domain.StoredObjectStatusStaging || stored.VerifiedSHA256 != nil || stored.VerifiedAt != nil {
				t.Fatal("写入确认或任务完整事实错误", err, readError, taskError)
			}
			if err := pool.QueryRowContext(ctx, "SELECT storage_quota,used_storage,reserved_storage FROM users WHERE id=7").Scan(&quotaAfter[0], &quotaAfter[1], &quotaAfter[2]); err != nil || quotaBefore != quotaAfter {
				t.Fatal("记录存储信息改变了配额事实", err)
			}
			if mode == "nil metadata" {
				if stored.StorageETag != nil || stored.StorageVersionID != nil {
					t.Fatal("NULL 元数据语义错误")
				}
			} else if stored.StorageETag == nil || *stored.StorageETag != etag || (mode != "preserve known" && (stored.StorageVersionID == nil || *stored.StorageVersionID != version)) {
				t.Fatal("实际元数据或已知字段保留错误")
			}
			if mode == "repeat" {
				repeated, err := record(current, 19, 23, 3, "current-worker", confirmation)
				if err != nil || !reflect.DeepEqual(result, repeated) {
					t.Fatal("重复确认改写了元数据或更新时间", err)
				}
			}
		})
	}
	for _, mode := range []string{"wrong object", "old lease", "wrong owner", "expired lease", "expired task", "wrong task stage", "wrong object stage", "conflicting metadata", "foreign task", "account disabled", "device revoked", "session revoked"} {
		t.Run(mode, func(t *testing.T) {
			reset(t)
			taskID, objectID, version, owner := uint64(19), uint64(23), uint64(3), "current-worker"
			want := domain.ErrUploadTaskBusy
			statement := ""
			switch mode {
			case "wrong object":
				objectID = 24
			case "old lease":
				version = 2
			case "wrong owner":
				owner = "old-worker"
			case "expired lease":
				statement = "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19"
			case "expired task":
				statement, want = "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19", domain.ErrUploadTaskNotReady
			case "wrong task stage":
				statement, want = "UPDATE upload_tasks SET status='VERIFYING' WHERE id=19", domain.ErrUploadTaskNotReady
			case "wrong object stage":
				statement, want = "UPDATE stored_objects SET status='VERIFYING' WHERE id=23", domain.ErrInvalidStoredObjectData
			case "conflicting metadata":
				statement, want = "UPDATE stored_objects SET storage_etag='existing-etag' WHERE id=23", domain.ErrInvalidStoredObjectData
			case "foreign task":
				taskID, objectID, want = 21, 24, domain.ErrUploadTaskNotFound
			case "account disabled":
				statement, want = "UPDATE users SET status='DISABLED' WHERE id=7", domain.ErrAccountDisabled
			case "device revoked":
				statement, want = "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=11", domain.ErrDeviceRevoked
			case "session revoked":
				statement, want = "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", domain.ErrSessionRevoked
			}
			if statement != "" {
				if _, err := pool.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil {
				t.Fatal(err)
			}
			etag := "new-etag"
			result, err := record(actor, taskID, objectID, version, owner, domain.UploadObjectResult{SizeBytes: domain.MaxSingleUploadSizeBytes, StorageETag: &etag})
			after, readError := store.FindUserStoredObject(ctx, 7, 23)
			if !errors.Is(err, want) || result != nil || readError != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("无效当前状态修改了对象或错误分类不符", err, readError)
			}
		})
	}
	for _, cancelAfterWrite := range []bool{false, true} {
		name := "outer rollback"
		if cancelAfterWrite {
			name = "cancel after write"
		}
		t.Run(name, func(t *testing.T) {
			reset(t)
			before, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil {
				t.Fatal(err)
			}
			current, stop := context.WithCancel(ctx)
			defer stop()
			fault := errors.New("later transaction step failed")
			etag := "rollback-etag"
			err = store.WithUserTransaction(current, actor, func(txContext context.Context, txStore *Store) error {
				if _, err := txStore.RecordUserUploadObjectWrite(txContext, 19, 23, 3, "current-worker", domain.UploadObjectResult{SizeBytes: domain.MaxSingleUploadSizeBytes, StorageETag: &etag}); err != nil {
					return err
				}
				if cancelAfterWrite {
					stop()
					return nil
				}
				return fault
			})
			want := fault
			if cancelAfterWrite {
				want = context.Canceled
			}
			after, readError := store.FindUserStoredObject(ctx, 7, 23)
			if !errors.Is(err, want) || readError != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("后续失败未完整回滚元数据", err, readError)
			}
		})
	}
	pool.SetMaxOpenConns(3)
	for _, sameResult := range []bool{true, false} {
		name := "concurrent same result"
		if !sameResult {
			name = "concurrent conflicting results"
		}
		t.Run(name, func(t *testing.T) {
			reset(t)
			var callers sync.WaitGroup
			proceed := make(chan struct{})
			results := make(chan error, 2)
			for index := range 2 {
				callers.Go(func() {
					<-proceed
					etag := "etag-a"
					if !sameResult && index == 1 {
						etag = "etag-b"
					}
					_, err := record(actor, 19, 23, 3, "current-worker", domain.UploadObjectResult{SizeBytes: domain.MaxSingleUploadSizeBytes, StorageETag: &etag})
					results <- err
				})
			}
			close(proceed)
			callers.Wait()
			close(results)
			successes, conflicts := 0, 0
			for err := range results {
				if err == nil {
					successes++
				} else if errors.Is(err, domain.ErrInvalidStoredObjectData) {
					conflicts++
				} else {
					t.Fatal(err)
				}
			}
			if (sameResult && (successes != 2 || conflicts != 0)) || (!sameResult && (successes != 1 || conflicts != 1)) {
				t.Fatal("并发元数据确认结果错误", successes, conflicts)
			}
		})
	}
	t.Run("maximum unsigned identities and version", func(t *testing.T) {
		maximum := ^uint64(0)
		for _, fixture := range []struct {
			statement string
			arguments []any
		}{
			{"INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'record_object_max','完整用户','fixture_hash')", []any{maximum}},
			{"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'e8ee758a-0727-42de-ae7d-3bb33155e148','完整设备','WEB')", []any{maximum, maximum}},
			{"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", []any{maximum, maximum, maximum}},
			{"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','record-max',0)", []any{maximum, maximum}},
			{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", []any{maximum, maximum}},
			{"INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,lease_owner,lease_until,lease_version,expires_at) VALUES(?,?,?,?,?,'maximum.bin','SINGLE',0,'UPLOADING',0,'current-worker',UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,?,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)", []any{maximum, maximum, maximum, maximum, maximum, maximum}},
		} {
			if _, err := pool.ExecContext(ctx, fixture.statement, fixture.arguments...); err != nil {
				t.Fatal(err)
			}
		}
		etag := "maximum-etag"
		result, err := record(domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}, maximum, maximum, maximum, "current-worker", domain.UploadObjectResult{StorageETag: &etag})
		if err != nil || result == nil || result.ID != maximum || result.OwnerUserID != maximum || result.StorageETag == nil || *result.StorageETag != etag {
			t.Fatal("完整无符号存储写入确认失败", err)
		}
	})
}
