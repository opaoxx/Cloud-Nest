package repository

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

// preparationFailureStore 在真实读取/写入后注入故障，验证服务事务闭环。
type preparationFailureStore struct {
	*Store
	stage   string
	failure error
	cancel  context.CancelFunc
}

func (store *preparationFailureStore) finish(stage string) error {
	if store.stage != stage {
		return nil
	}
	if store.cancel != nil {
		store.cancel()
	}
	return store.failure
}

func (store *preparationFailureStore) LockUserUploadTask(ctx context.Context, taskID uint64) (*domain.UploadTask, error) {
	task, err := store.Store.LockUserUploadTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return task, store.finish("lock")
}

func (store *preparationFailureStore) FindUserStoredObject(ctx context.Context, userID, objectID uint64) (*domain.StoredObject, error) {
	object, err := store.Store.FindUserStoredObject(ctx, userID, objectID)
	if err != nil {
		return nil, err
	}
	return object, store.finish("read")
}

func (store *preparationFailureStore) ClaimUserUploadContentLease(ctx context.Context, taskID, objectID, version uint64, owner string) (*domain.UploadTask, error) {
	task, err := store.Store.ClaimUserUploadContentLease(ctx, taskID, objectID, version, owner)
	if err != nil {
		return nil, err
	}
	return task, store.finish("claim")
}

func (store *preparationFailureStore) CreateUserStagedObject(ctx context.Context, input domain.CreateStagedObjectInput) (*domain.StoredObject, error) {
	object, err := store.Store.CreateUserStagedObject(ctx, input)
	if err != nil {
		return nil, err
	}
	return object, store.finish("create")
}

func (store *preparationFailureStore) ReplaceUserUploadStagedObject(ctx context.Context, taskID, oldID, newID, version uint64, owner string) (*domain.UploadTask, error) {
	task, err := store.Store.ReplaceUserUploadStagedObject(ctx, taskID, oldID, newID, version, owner)
	if err != nil {
		return nil, err
	}
	return task, store.finish("replace")
}

func (store *preparationFailureStore) MarkUserStagedObjectOrphaned(ctx context.Context, objectID uint64) (*domain.StoredObject, error) {
	object, err := store.Store.MarkUserStagedObjectOrphaned(ctx, objectID)
	if err != nil {
		return nil, err
	}
	return object, store.finish("orphan")
}

func TestUploadContentPreparationServiceIntegration(t *testing.T) {
	// 1. 准备独占库中的正式记录、可信身份和现有任务。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表定义未找到")
	}
	for _, statement := range []string{
		migrations.InitialSQL[start:end],
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(15,7,'e8ee758a-0727-42de-ae7d-3bb33155e148','其他设备','WEB')",
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
	failure := errors.New("preparation rollback fixture")
	newPreparer := func(stage string, cancelCall context.CancelFunc) *service.UploadContentPreparationService {
		return service.NewUploadContentPreparationService("local_minio", "cloudnest", func(ctx context.Context, current domain.Actor, action func(context.Context, service.UploadContentPreparationTransaction) error) error {
			return store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
				return action(txContext, &preparationFailureStore{Store: txStore, stage: stage, failure: failure, cancel: cancelCall})
			})
		})
	}
	preparer := newPreparer("", nil)
	reset := func(t *testing.T, version uint64) {
		t.Helper()
		for _, statement := range []string{
			"UPDATE upload_tasks SET staged_object_id=23,upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,status='UPLOADING',file_size=16777216,reserved_bytes=16777216,lease_owner=NULL,lease_until=NULL,lease_version=0,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=19",
			"UPDATE stored_objects SET status='STAGING',file_size=16777216,storage_version_id=NULL,storage_etag=NULL,verified_sha256=NULL,verified_at=NULL,unreferenced_at=NULL,gc_after=NULL,last_error_code=NULL WHERE id=23",
			"UPDATE users SET reserved_storage=16777216 WHERE id=7",
		} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET lease_version=? WHERE id=19", version); err != nil {
			t.Fatal(err)
		}
	}
	type preparationState struct{ objects, tasks, reserved, used uint64 }
	readState := func(t *testing.T) preparationState {
		t.Helper()
		var state preparationState
		if err := pool.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM stored_objects WHERE owner_user_id=7),(SELECT COUNT(*) FROM upload_tasks WHERE user_id=7),reserved_storage,used_storage FROM users WHERE id=7").Scan(&state.objects, &state.tasks, &state.reserved, &state.used); err != nil {
			t.Fatal(err)
		}
		return state
	}
	for _, mode := range []string{"first", "retry", "zero bytes", "no digest", "other device", "retry written object", "last claimable version"} {
		t.Run(mode, func(t *testing.T) {
			// 2. 单连接完成首次/重试准备，预占沿用任务已有声明。
			version, current := uint64(0), actor
			if mode == "retry" || mode == "retry written object" {
				version = 3
			}
			if mode == "last claimable version" {
				version = ^uint64(0) - 1
			}
			reset(t, version)
			switch mode {
			case "zero bytes":
				for _, statement := range []string{"UPDATE upload_tasks SET file_size=0,reserved_bytes=0 WHERE id=19", "UPDATE stored_objects SET file_size=0 WHERE id=23", "UPDATE users SET reserved_storage=0 WHERE id=7"} {
					if _, err := pool.ExecContext(ctx, statement); err != nil {
						t.Fatal(err)
					}
				}
			case "no digest":
				if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET expected_sha256=NULL WHERE id=19"); err != nil {
					t.Fatal(err)
				}
			case "other device":
				current = domain.Actor{UserID: 7, DeviceID: 15, SessionID: 18}
			case "retry written object":
				if _, err := pool.ExecContext(ctx, "UPDATE stored_objects SET storage_version_id='old-version',storage_etag='old-etag' WHERE id=23"); err != nil {
					t.Fatal(err)
				}
			}
			before := readState(t)
			pool.SetMaxOpenConns(1)
			result, err := preparer.Prepare(ctx, current, 19)
			pool.SetMaxOpenConns(3)
			if err != nil {
				t.Fatal("真实接收准备失败", err)
			}
			identifier, parseError := uuid.Parse(*result.Task.LeaseOwner)
			if parseError != nil || identifier.Version() != 4 || result.Task.LeaseVersion != version+1 || result.Task.DeviceID != 11 || result.Object.ID != result.Task.StagedObjectID {
				t.Fatal("准备租约、来源设备或对象关联错误", parseError)
			}
			after := readState(t)
			wantObjects := before.objects
			if version == 0 {
				if result.Object.ID != 23 {
					t.Fatal("首次准备未使用初始对象")
				}
			} else {
				wantObjects++
				old, err := store.FindUserStoredObject(ctx, 7, 23)
				if err != nil || old.Status != domain.StoredObjectStatusOrphaned || old.GCAfter == nil || !old.GCAfter.Equal(old.UnreferencedAt.Add(7*24*time.Hour)) ||
					result.Object.ID == 23 || result.Object.Provider != "local_minio" || result.Object.Bucket != "cloudnest" || !strings.HasPrefix(result.Object.ObjectKey, "users/7/objects/") {
					t.Fatal("重试对象与旧对象宽限期错误", err)
				}
			}
			if after.objects != wantObjects || after.tasks != before.tasks || after.reserved != before.reserved || after.used != before.used {
				t.Fatal("准备计数或空间事实错误")
			}
			persisted, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil || !reflect.DeepEqual(persisted, result.Task) {
				t.Fatal("准备任务结果未提交", err)
			}
		})
	}
	t.Run("active lease and scope", func(t *testing.T) {
		reset(t, 0)
		if _, err := preparer.Prepare(ctx, actor, 19); err != nil {
			t.Fatal(err)
		}
		before := readState(t)
		for _, fixture := range []struct {
			taskID uint64
			want   error
		}{{19, domain.ErrUploadTaskBusy}, {21, domain.ErrUploadTaskNotFound}, {999, domain.ErrUploadTaskNotFound}} {
			if result, err := preparer.Prepare(ctx, actor, fixture.taskID); result != nil || !errors.Is(err, fixture.want) {
				t.Fatal("有效占用或任务归属未拒绝", err)
			}
		}
		if readState(t) != before {
			t.Fatal("拒绝准备改变了业务计数")
		}
	})
	for _, fixture := range []struct {
		name, change, restore string
		wantError             error
	}{
		{"disabled", "UPDATE users SET status='DISABLED' WHERE id=7", "UPDATE users SET status='ACTIVE' WHERE id=7", domain.ErrAccountDisabled},
		{"revoked device", "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=11", "UPDATE user_devices SET revoked_at=NULL WHERE id=11", domain.ErrDeviceRevoked},
		{"revoked session", "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		// 会话校验使用应用时钟，固定过去时间避免依赖容器与主机的瞬时钟差。
		{"expired session", "UPDATE auth_sessions SET expires_at='2000-01-01 00:00:00' WHERE id=17", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=17", domain.ErrSessionExpired},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			reset(t, 0)
			if _, err := pool.ExecContext(ctx, fixture.change); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := pool.ExecContext(ctx, fixture.restore); err != nil {
					t.Error(err)
				}
			}()
			before := readState(t)
			if result, err := preparer.Prepare(ctx, actor, 19); result != nil || !errors.Is(err, fixture.wantError) || readState(t) != before {
				t.Fatal("失效身份仍完成接收准备", err)
			}
		})
	}
	for _, mode := range []string{"failure", "cancelled"} {
		for _, stage := range []string{"lock", "claim", "read", "create", "replace", "orphan"} {
			t.Run(mode+" after "+stage, func(t *testing.T) {
				// 3. 在真实阶段完成后注入失败，完整结果共同回滚。
				reset(t, 3)
				before := readState(t)
				beforeTask, err := store.FindUserUploadTask(ctx, 7, 19)
				if err != nil {
					t.Fatal(err)
				}
				beforeObject, err := store.FindUserStoredObject(ctx, 7, 23)
				if err != nil {
					t.Fatal(err)
				}
				requestContext, stop := context.WithCancel(ctx)
				var cancelCall context.CancelFunc
				wantError := failure
				if mode == "cancelled" {
					cancelCall, wantError = stop, context.Canceled
				}
				result, err := newPreparer(stage, cancelCall).Prepare(requestContext, actor, 19)
				stop()
				if result != nil || !errors.Is(err, wantError) {
					t.Fatal("阶段失败或取消交付错误", err)
				}
				afterTask, taskError := store.FindUserUploadTask(ctx, 7, 19)
				afterObject, objectError := store.FindUserStoredObject(ctx, 7, 23)
				if taskError != nil || objectError != nil || readState(t) != before || !reflect.DeepEqual(beforeTask, afterTask) || !reflect.DeepEqual(beforeObject, afterObject) {
					t.Fatal("阶段失败未完整回滚", taskError, objectError)
				}
			})
		}
	}
	for _, mode := range []string{"first", "retry"} {
		t.Run("concurrent "+mode, func(t *testing.T) {
			version := uint64(0)
			if mode == "retry" {
				version = 3
			}
			reset(t, version)
			before := readState(t)
			var workers sync.WaitGroup
			start := make(chan struct{})
			results := make(chan error, 2)
			for range 2 {
				workers.Add(1)
				go func() {
					defer workers.Done()
					<-start
					_, err := preparer.Prepare(ctx, actor, 19)
					results <- err
				}()
			}
			close(start)
			workers.Wait()
			close(results)
			successes, busy := 0, 0
			for err := range results {
				if err == nil {
					successes++
				} else if errors.Is(err, domain.ErrUploadTaskBusy) {
					busy++
				} else {
					t.Fatal(err)
				}
			}
			wantObjects := before.objects
			if version > 0 {
				wantObjects++
			}
			if successes != 1 || busy != 1 || readState(t).objects != wantObjects {
				t.Fatal("并发准备未产生唯一当前接收尝试")
			}
		})
	}
	t.Run("maximum identities and final lease version", func(t *testing.T) {
		maximum := ^uint64(0)
		oldObjectID := uint64(1)<<63 + 100
		for _, fixture := range []struct {
			statement string
			arguments []any
		}{
			{"INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'prepare_content_max','完整用户','fixture_hash')", []any{maximum}},
			{"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'f8ee758a-0727-42de-ae7d-3bb33155e148','完整设备','WEB')", []any{maximum, maximum}},
			{"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", []any{maximum, maximum, maximum}},
			{"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','prepare-max-old',0)", []any{oldObjectID, maximum}},
			{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", []any{maximum, maximum}},
			{"INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,lease_version,expires_at) VALUES(?,?,?,?,?,'maximum.bin','SINGLE',0,'UPLOADING',0,?,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)", []any{maximum, maximum, maximum, oldObjectID, maximum, maximum - 1}},
		} {
			if _, err := pool.ExecContext(ctx, fixture.statement, fixture.arguments...); err != nil {
				t.Fatal(err)
			}
		}
		result, err := preparer.Prepare(ctx, domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}, maximum)
		if err != nil || result.Task.ID != maximum || result.Task.UserID != maximum || result.Task.LeaseVersion != maximum || result.Object.ID != oldObjectID+1 ||
			!strings.HasPrefix(result.Object.ObjectKey, "users/"+strconv.FormatUint(maximum, 10)+"/objects/") {
			t.Fatal("完整无符号准备链失败", err)
		}
	})
	t.Run("maximum object first receive and exhausted allocation rollback", func(t *testing.T) {
		maximum := ^uint64(0)
		current := domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','maximum-object-first',0)", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at) VALUES(?,?,?,?,?,'maximum-first.bin','SINGLE',0,'UPLOADING',0,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)", maximum-1, maximum, maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		result, err := preparer.Prepare(ctx, current, maximum-1)
		if err != nil || result.Object.ID != maximum || result.Task.LeaseVersion != 1 {
			t.Fatal("最大对象标识首次接收失败", err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE user_id=? AND id=?", maximum, maximum-1); err != nil {
			t.Fatal(err)
		}
		before, err := store.FindUserUploadTask(ctx, maximum, maximum-1)
		if err != nil {
			t.Fatal(err)
		}
		result, err = preparer.Prepare(ctx, current, maximum-1)
		var databaseError *mysql.MySQLError
		if result != nil || !errors.As(err, &databaseError) || databaseError.Number != 1467 {
			t.Fatal("对象标识耗尽错误丢失", err)
		}
		after, err := store.FindUserUploadTask(ctx, maximum, maximum-1)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("分配失败未回滚领取的租约", err)
		}
		object, err := store.FindUserStoredObject(ctx, maximum, maximum)
		if err != nil || object.Status != domain.StoredObjectStatusStaging || object.UnreferencedAt != nil {
			t.Fatal("分配失败错误孤立了原对象", err)
		}
	})
}
