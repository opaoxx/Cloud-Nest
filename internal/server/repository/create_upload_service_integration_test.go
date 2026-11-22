package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

// createUploadServiceStore 在真实写入后注入失败，核对完整服务的事务回滚。
type createUploadServiceStore struct {
	*Store
	failStage string
	failure   error
	cancel    context.CancelFunc
}

func (store *createUploadServiceStore) finishCreateStep(stage string) error {
	if store.failStage != stage {
		return nil
	}
	if store.cancel != nil {
		store.cancel()
	}
	return store.failure
}

func (store *createUploadServiceStore) ReserveUserUploadSpace(ctx context.Context, sizeBytes uint64) error {
	if err := store.Store.ReserveUserUploadSpace(ctx, sizeBytes); err != nil {
		return err
	}
	return store.finishCreateStep("reserve")
}

func (store *createUploadServiceStore) CreateUserStagedObject(ctx context.Context, input domain.CreateStagedObjectInput) (*domain.StoredObject, error) {
	object, err := store.Store.CreateUserStagedObject(ctx, input)
	if err != nil {
		return nil, err
	}
	return object, store.finishCreateStep("object")
}

func (store *createUploadServiceStore) CreateUserUploadTask(ctx context.Context, objectID uint64, input domain.CreateUploadInput, expiresAt time.Time) (*domain.UploadTask, error) {
	task, err := store.Store.CreateUserUploadTask(ctx, objectID, input, expiresAt)
	if err != nil {
		return nil, err
	}
	return task, store.finishCreateStep("task")
}

func (store *createUploadServiceStore) CreateUserMultipartUploadTask(ctx context.Context, objectID uint64, input domain.CreateUploadInput, plan domain.UploadPlan, expiresAt time.Time) (*domain.UploadTask, error) {
	task, err := store.Store.CreateUserMultipartUploadTask(ctx, objectID, input, plan, expiresAt)
	if err != nil {
		return nil, err
	}
	return task, store.finishCreateStep("task")
}

func TestCreateUploadServiceIntegration(t *testing.T) {
	// 1. 使用独占库中的正式表，准备两位用户的授权、目录和配额事实。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	for _, boundary := range []struct{ table, next string }{{"auth_sessions", "stored_objects"}, {"upload_tasks", "idempotency_records"}} {
		start := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.table+" (")
		end := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.next+" (")
		if start < 0 || end <= start {
			t.Fatal("正式表边界错误")
		}
		if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	installUploadReplacementFixtureColumns(t, ctx, pool)
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,storage_quota) VALUES(1,'create_upload_owner','上传用户','fixture_hash',104857600),(3,'create_upload_other','另一用户','fixture_hash',104857600)",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','上传设备','WEB'),(31,3,'38ee758a-0727-42de-ae7d-3bb33155e148','另一设备','WINDOWS')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),(37,3,31,UNHEX(REPEAT('33',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'target',X'746172676574','DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(15,1,11,'hidden',X'68696464656e','DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(16,1,15,'child',X'6368696c64','DIR')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'local_minio','cloudnest','fixture/file',1)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(14,1,11,'file.bin',X'66696c652e62696e','FILE','OTHER',41,1)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 13, SessionID: 17}
	other := domain.Actor{UserID: 3, DeviceID: 31, SessionID: 37}
	failure := errors.New("fixture creation rollback")
	newCreator := func(failStage string, cancelCall context.CancelFunc) *service.CreateUploadService {
		return service.NewCreateUploadService("local_minio", "cloudnest", func(ctx context.Context, current domain.Actor, action func(context.Context, service.CreateUploadTransaction) error) error {
			return store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
				return action(txContext, &createUploadServiceStore{Store: txStore, failStage: failStage, failure: failure, cancel: cancelCall})
			})
		})
	}
	creator := newCreator("", nil)
	inputFor := func() domain.CreateUploadInput {
		return domain.CreateUploadInput{ParentID: 12, Name: " 学习.bin ", SizeBytes: 40}
	}
	type uploadState struct{ reserved, taskReserved, objects, tasks, used, sequence uint64 }
	readState := func(userID uint64) uploadState {
		var state uploadState
		err := pool.QueryRowContext(ctx, "SELECT reserved_storage,(SELECT COALESCE(SUM(reserved_bytes),0) FROM upload_tasks WHERE user_id=users.id),(SELECT COUNT(*) FROM stored_objects WHERE owner_user_id=users.id),(SELECT COUNT(*) FROM upload_tasks WHERE user_id=users.id),used_storage,change_seq FROM users WHERE id=?", userID).Scan(&state.reserved, &state.taskReserved, &state.objects, &state.tasks, &state.used, &state.sequence)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}

	// 2. 单连接通过真实核心服务完成声明、路径、预占、对象和任务的整体提交。
	t.Run("one connection complete creation", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		for _, mode := range []string{"ordinary", "zero bytes", "digest", "zero digest", "single limit", "root parent", "same display name"} {
			input := inputFor()
			switch mode {
			case "zero bytes":
				input.SizeBytes = 0
			case "digest":
				digest := sha256.Sum256([]byte("fixture-content"))
				input.ExpectedSHA256 = &digest
			case "zero digest":
				input.ExpectedSHA256 = new([sha256.Size]byte)
			case "single limit":
				input.SizeBytes = domain.MaxSingleUploadSizeBytes
			case "root parent":
				input.ParentID = 11
			}
			before := readState(actor.UserID)
			earliest := time.Now().UTC().Truncate(time.Microsecond).Add(24 * time.Hour)
			task, err := creator.Create(ctx, actor, input)
			if err != nil || task == nil || task.TargetParentID != input.ParentID || task.RequestedName != "学习.bin" || task.ExpiresAt.Before(earliest) || task.ExpiresAt.After(time.Now().Add(24*time.Hour)) || task.ExpiresAt.Location() != time.UTC || task.ExpiresAt.Nanosecond()%1000 != 0 || !reflect.DeepEqual(task.ExpectedSHA256, input.ExpectedSHA256) {
				t.Fatal("完整服务任务声明或有效期错误", err)
			}
			object, err := store.FindUserStoredObject(ctx, actor.UserID, task.StagedObjectID)
			if err != nil || object == nil || !strings.HasPrefix(object.ObjectKey, "users/1/objects/") || object.FileSize != input.SizeBytes || object.Status != domain.StoredObjectStatusStaging {
				t.Fatal("完整服务对象位置错误", err)
			}
			again, err := store.FindUserUploadTask(ctx, actor.UserID, task.ID)
			if err != nil || !reflect.DeepEqual(task, again) {
				t.Fatal("任务提交后回读错误", err)
			}
			after := readState(actor.UserID)
			if after.reserved != before.reserved+input.SizeBytes || after.reserved != after.taskReserved || after.objects != before.objects+1 || after.tasks != before.tasks+1 || after.used != before.used || after.sequence != before.sequence {
				t.Fatal("配额、对象和任务提交事实不一致")
			}
			task.RequestedName = "changed"
			if task.ExpectedSHA256 != nil {
				task.ExpectedSHA256[0] ^= 0xff
			}
			fresh, err := store.FindUserUploadTask(ctx, actor.UserID, task.ID)
			if err != nil || !reflect.DeepEqual(fresh, again) {
				t.Fatal("修改任务结果影响已提交数据", err)
			}
		}
	})
	t.Run("separate users", func(t *testing.T) {
		before := readState(actor.UserID)
		input := inputFor()
		input.ParentID = 33
		task, err := creator.Create(ctx, other, input)
		if err != nil || task == nil || task.UserID != 3 || task.DeviceID != 31 || readState(actor.UserID) != before || readState(other.UserID).reserved != input.SizeBytes {
			t.Fatal("其他用户创建影响当前用户", err)
		}
		if foreign, err := store.FindUserUploadTask(ctx, actor.UserID, task.ID); foreign != nil || !errors.Is(err, domain.ErrUploadTaskNotFound) {
			t.Fatal("跨用户读取任务未隔离", err)
		}
	})
	t.Run("invalid input and target visibility", func(t *testing.T) {
		before := readState(actor.UserID)
		for _, parent := range []uint64{999, 33, 15, 16, 14} {
			input := inputFor()
			input.ParentID = parent
			task, err := creator.Create(ctx, actor, input)
			want := domain.ErrFileEntryNotFound
			if parent == 14 {
				want = domain.ErrFileEntryNotDirectory
			}
			if task != nil || !errors.Is(err, want) || readState(actor.UserID) != before {
				t.Fatal("目标不可用后产生了创建事实", err)
			}
		}
		for _, mode := range []string{"zero parent", "bad name", "large file"} {
			input := inputFor()
			switch mode {
			case "zero parent":
				input.ParentID = 0
			case "bad name":
				input.Name = "private/name"
			case "large file":
				input.SizeBytes = domain.MaxSingleUploadSizeBytes + 1
			}
			if task, err := creator.Create(ctx, actor, input); task != nil || err == nil || readState(actor.UserID) != before {
				t.Fatal("非法声明仍产生了写入", err)
			}
		}
	})

	// 3. 在每个真实写入阶段后注入失败和取消，三项创建事实一起回滚。
	for _, stage := range []string{"reserve", "object", "task"} {
		for _, cancelled := range []bool{false, true} {
			t.Run(stage+" rollback cancelled="+strconv.FormatBool(cancelled), func(t *testing.T) {
				before := readState(actor.UserID)
				callContext, cancelCall := context.WithCancel(ctx)
				defer cancelCall()
				var cancellation context.CancelFunc
				if cancelled {
					cancellation = cancelCall
				}
				task, err := newCreator(stage, cancellation).Create(callContext, actor, inputFor())
				want := failure
				if cancelled {
					want = context.Canceled
				}
				if task != nil || !errors.Is(err, want) || readState(actor.UserID) != before {
					t.Fatal("服务失败后预占、对象或任务未整体回滚", err)
				}
			})
		}
	}
	for _, fixture := range []struct {
		name, change, restore string
		want                  error
	}{
		{"disabled account", "UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
		{"revoked device", "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=13", "UPDATE user_devices SET revoked_at=NULL WHERE id=13", domain.ErrDeviceRevoked},
		{"revoked session", "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		{"expired session", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=17", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=17", domain.ErrSessionExpired},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			before := readState(actor.UserID)
			if _, err := pool.ExecContext(ctx, fixture.change); err != nil {
				t.Fatal(err)
			}
			task, err := creator.Create(ctx, actor, inputFor())
			if _, restoreError := pool.ExecContext(ctx, fixture.restore); restoreError != nil {
				t.Fatal(restoreError)
			}
			if task != nil || !errors.Is(err, fixture.want) || readState(actor.UserID) != before {
				t.Fatal("授权状态失效后产生了创建事实", err)
			}
		})
	}
	t.Run("multipart commit and rollback", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		limits := domain.UploadPlanLimits{
			MaxFileSizeBytes: domain.DefaultMaxUploadSizeBytes, MaxObjectSizeBytes: domain.DefaultMaxUploadSizeBytes,
			MaxPartSizeBytes: 64 * 1024 * 1024,
		}
		digest := sha256.Sum256([]byte("multipart fixture"))
		input := inputFor()
		input.SizeBytes, input.ExpectedSHA256 = domain.MaxSingleUploadSizeBytes+1, &digest

		// 1. 提交后从独占库核对配额、对象和任务的状态。
		before := readState(actor.UserID)
		task, err := creator.CreateWithPlan(ctx, actor, input, limits)
		if err != nil || task == nil || task.UserID != actor.UserID || task.UploadMode != "MULTIPART" ||
			task.Status != domain.UploadStatusCreating || task.StorageUploadID != nil || task.ReservedBytes != input.SizeBytes ||
			task.PartSize == nil || *task.PartSize != 16777216 || task.PartCount == nil || *task.PartCount != 2 {
			t.Fatalf("事务提交未返回固定计划: %+v %v", task, err)
		}
		after := readState(actor.UserID)
		if after.reserved != before.reserved+input.SizeBytes || after.reserved != after.taskReserved ||
			after.objects != before.objects+1 || after.tasks != before.tasks+1 || after.used != before.used || after.sequence != before.sequence {
			t.Fatalf("配额、对象和任务未一起提交: before=%+v after=%+v", before, after)
		}
		object, err := store.FindUserStoredObject(ctx, actor.UserID, task.StagedObjectID)
		if err != nil || object == nil || object.FileSize != input.SizeBytes || object.Status != domain.StoredObjectStatusStaging {
			t.Fatal("分片任务未引用相同大小的暂存对象", err)
		}
		persisted, err := store.FindUserUploadTask(ctx, actor.UserID, task.ID)
		if err != nil || !reflect.DeepEqual(persisted, task) {
			t.Fatal("提交后读回的任务与结果不同", err)
		}

		// 2. 在三项真实写入后分别注入错误，任务写入后另外验证取消。
		for _, stage := range []string{"reserve", "object", "task"} {
			task, err := newCreator(stage, nil).CreateWithPlan(ctx, actor, input, limits)
			if task != nil || !errors.Is(err, failure) || readState(actor.UserID) != after {
				t.Fatalf("%s 后失败没有整体回滚: %v", stage, err)
			}
		}
		cancelContext, cancel := context.WithCancel(ctx)
		defer cancel()
		task, err = newCreator("task", cancel).CreateWithPlan(cancelContext, actor, input, limits)
		if task != nil || !errors.Is(err, context.Canceled) || readState(actor.UserID) != after {
			t.Fatal("任务写入后取消没有整体回滚", err)
		}

		// 3. 祖先不可见、配额不足或摘要缺失时保持创建前的记录。
		badTarget, tooLarge, missingDigest := input, input, input
		badTarget.ParentID = 16
		tooLarge.SizeBytes = 104857601
		missingDigest.ExpectedSHA256 = nil
		for _, fixture := range []struct {
			input domain.CreateUploadInput
			want  error
		}{
			{badTarget, domain.ErrFileEntryNotFound},
			{tooLarge, domain.ErrQuotaExceeded},
			{missingDigest, domain.ErrMultipartUploadSHA256Required},
		} {
			task, err := creator.CreateWithPlan(ctx, actor, fixture.input, limits)
			if task != nil || !errors.Is(err, fixture.want) || readState(actor.UserID) != after {
				t.Fatalf("无效声明或目标改变了创建记录: %v，预期 %v", err, fixture.want)
			}
		}
	})
	t.Run("concurrent quota protects complete creation", func(t *testing.T) {
		before := readState(actor.UserID)
		if _, err := pool.ExecContext(ctx, "UPDATE users SET storage_quota=? WHERE id=1", before.reserved+100); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		var workers sync.WaitGroup
		for attempt := 0; attempt < 2; attempt++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				input := inputFor()
				input.SizeBytes = 60
				_, err := creator.Create(ctx, actor, input)
				results <- err
			}()
		}
		close(start)
		workers.Wait()
		close(results)
		successes, quotaFailures := 0, 0
		for err := range results {
			if err == nil {
				successes++
			} else if errors.Is(err, domain.ErrQuotaExceeded) {
				quotaFailures++
			} else {
				t.Fatal(err)
			}
		}
		after := readState(actor.UserID)
		if successes != 1 || quotaFailures != 1 || after.reserved != before.reserved+60 || after.reserved != after.taskReserved || after.objects != before.objects+1 || after.tasks != before.tasks+1 {
			t.Fatal("并发完整创建超额或残留写入")
		}
	})
	t.Run("maximum identities", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash,storage_quota) VALUES(?,'create_upload_max','完整身份','fixture_hash',?)", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'48ee758a-0727-42de-ae7d-3bb33155e148','完整设备','WINDOWS')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"ALTER TABLE stored_objects AUTO_INCREMENT = 9223372036854775810", "ALTER TABLE upload_tasks AUTO_INCREMENT = 9223372036854775810"} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		input := inputFor()
		input.ParentID = maximum
		task, err := creator.Create(ctx, domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}, input)
		if err != nil || task == nil || task.ID < uint64(1)<<63 || task.StagedObjectID < uint64(1)<<63 || task.UserID != maximum || task.DeviceID != maximum || task.TargetParentID != maximum {
			t.Fatal("完整身份或高位主键丢失", err)
		}
		object, err := store.FindUserStoredObject(ctx, maximum, task.StagedObjectID)
		state := readState(maximum)
		if err != nil || !strings.HasPrefix(object.ObjectKey, "users/18446744073709551615/objects/") || state.reserved != input.SizeBytes || state.reserved != state.taskReserved {
			t.Fatal("完整身份对象键或配额错误", err)
		}
	})
}
