package service

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type uploadContentPreparationTransactionStub struct {
	task         *domain.UploadTask
	objects      map[uint64]*domain.StoredObject
	now          time.Time
	newObjectID  uint64
	events       []string
	contexts     []context.Context
	input        domain.CreateStagedObjectInput
	failureStage string
	failure      error
	nilStage     string
	after        func(string, *domain.UploadTask, *domain.StoredObject)
}

func (transaction *uploadContentPreparationTransactionStub) finish(ctx context.Context, stage string, task *domain.UploadTask, object *domain.StoredObject) error {
	transaction.events = append(transaction.events, stage)
	transaction.contexts = append(transaction.contexts, ctx)
	if transaction.after != nil {
		transaction.after(stage, task, object)
	}
	if transaction.failureStage == stage {
		return transaction.failure
	}
	return nil
}

func (transaction *uploadContentPreparationTransactionStub) LockUserUploadTask(ctx context.Context, taskID uint64) (*domain.UploadTask, error) {
	err := transaction.finish(ctx, "lock", transaction.task, nil)
	if transaction.nilStage == "lock" {
		return nil, err
	}
	return transaction.task, err
}

func (transaction *uploadContentPreparationTransactionStub) ClaimUserUploadContentLease(ctx context.Context, taskID, objectID, version uint64, owner string) (*domain.UploadTask, error) {
	if taskID != transaction.task.ID || objectID != transaction.task.StagedObjectID || version != transaction.task.LeaseVersion {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if err := ValidateUploadContentLease(transaction.task, transaction.task.UserID, taskID, transaction.now); err != nil {
		return nil, err
	}
	transaction.task.LeaseOwner = copyOptionalValue(&owner)
	until := transaction.now.Add(time.Minute)
	transaction.task.LeaseUntil, transaction.task.LeaseVersion, transaction.task.UpdatedAt = &until, version+1, transaction.now
	err := transaction.finish(ctx, "claim", transaction.task, nil)
	if transaction.nilStage == "claim" {
		return nil, err
	}
	return transaction.task, err
}

func (transaction *uploadContentPreparationTransactionStub) FindUserStoredObject(ctx context.Context, userID, objectID uint64) (*domain.StoredObject, error) {
	object := transaction.objects[objectID]
	err := transaction.finish(ctx, "read", nil, object)
	if transaction.nilStage == "read" {
		return nil, err
	}
	return object, err
}

func (transaction *uploadContentPreparationTransactionStub) CreateUserStagedObject(ctx context.Context, input domain.CreateStagedObjectInput) (*domain.StoredObject, error) {
	transaction.input = input
	object := &domain.StoredObject{ID: transaction.newObjectID, OwnerUserID: transaction.task.UserID, Provider: input.Provider, Bucket: input.Bucket, ObjectKey: input.ObjectKey, FileSize: input.SizeBytes, Status: domain.StoredObjectStatusStaging, CreatedAt: transaction.now, UpdatedAt: transaction.now}
	transaction.objects[object.ID] = object
	err := transaction.finish(ctx, "create", nil, object)
	if transaction.nilStage == "create" {
		return nil, err
	}
	return object, err
}

func (transaction *uploadContentPreparationTransactionStub) ReplaceUserUploadStagedObject(ctx context.Context, taskID, oldID, newID, version uint64, owner string) (*domain.UploadTask, error) {
	if transaction.task.ID != taskID || transaction.task.StagedObjectID != oldID || transaction.task.LeaseVersion != version || *transaction.task.LeaseOwner != owner {
		return nil, domain.ErrUploadTaskBusy
	}
	transaction.task.StagedObjectID, transaction.task.UpdatedAt = newID, transaction.now.Add(time.Microsecond)
	err := transaction.finish(ctx, "replace", transaction.task, nil)
	if transaction.nilStage == "replace" {
		return nil, err
	}
	return transaction.task, err
}

func (transaction *uploadContentPreparationTransactionStub) MarkUserStagedObjectOrphaned(ctx context.Context, objectID uint64) (*domain.StoredObject, error) {
	if transaction.task.StagedObjectID == objectID {
		return nil, domain.ErrStoredObjectInUse
	}
	object := transaction.objects[objectID]
	stamp, gcAfter := transaction.now, transaction.now.Add(7*24*time.Hour)
	object.Status, object.UnreferencedAt, object.GCAfter, object.UpdatedAt = domain.StoredObjectStatusOrphaned, &stamp, &gcAfter, stamp
	err := transaction.finish(ctx, "orphan", nil, object)
	if transaction.nilStage == "orphan" {
		return nil, err
	}
	return object, err
}

func uploadContentPreparationFixture() (domain.Actor, *uploadContentPreparationTransactionStub) {
	task, now := uploadContentStateFixture()
	object := &domain.StoredObject{ID: task.StagedObjectID, OwnerUserID: task.UserID, Provider: "MINIO", Bucket: "fixture-bucket", ObjectKey: "old-attempt", FileSize: task.FileSize, Status: domain.StoredObjectStatusStaging, CreatedAt: task.CreatedAt, UpdatedAt: task.CreatedAt}
	return domain.Actor{UserID: task.UserID, DeviceID: task.DeviceID, SessionID: 17}, &uploadContentPreparationTransactionStub{task: &task, now: now, newObjectID: 29, objects: map[uint64]*domain.StoredObject{object.ID: object}}
}

func TestUploadContentPreparationServiceResults(t *testing.T) {
	for _, mode := range []string{"first", "retry", "zero bytes", "single limit", "other device", "retry written object", "last claimable version", "maximum identifiers", "no digest", "snapshots"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备首次或已执行过的接收任务。
			actor, transaction := uploadContentPreparationFixture()
			oldID := transaction.task.StagedObjectID
			if mode == "retry" || mode == "retry written object" {
				transaction.task.LeaseVersion = 3
			}
			switch mode {
			case "zero bytes":
				transaction.task.FileSize, transaction.task.ReservedBytes, transaction.objects[oldID].FileSize = 0, 0, 0
			case "single limit":
				transaction.task.FileSize, transaction.task.ReservedBytes, transaction.objects[oldID].FileSize = domain.MaxSingleUploadSizeBytes, domain.MaxSingleUploadSizeBytes, domain.MaxSingleUploadSizeBytes
			case "other device":
				actor.DeviceID = 15
			case "retry written object":
				etag := "old-etag"
				transaction.objects[oldID].StorageETag = &etag
			case "last claimable version":
				transaction.task.LeaseVersion = ^uint64(0) - 1
			case "maximum identifiers":
				actor.UserID, actor.DeviceID, actor.SessionID = ^uint64(0), ^uint64(0), ^uint64(0)
				transaction.task.ID, transaction.task.UserID, transaction.task.DeviceID, transaction.task.TargetParentID = ^uint64(0), actor.UserID, actor.DeviceID, ^uint64(0)
				transaction.objects[oldID].OwnerUserID = actor.UserID
			case "no digest":
				transaction.task.ExpectedSHA256 = nil
			}
			original := copyUploadTask(transaction.task)
			txContext := context.WithValue(context.Background(), struct{}{}, "transaction")
			calls := 0
			preparer := NewUploadContentPreparationService("MINIO", "fixture-bucket", func(ctx context.Context, current domain.Actor, action func(context.Context, UploadContentPreparationTransaction) error) error {
				calls++
				if current != actor {
					t.Error("可信 Actor 未传入事务")
				}
				err := action(txContext, transaction)
				if mode == "snapshots" && err == nil {
					*transaction.task.LeaseOwner, transaction.task.RequestedName, transaction.objects[oldID].ObjectKey = "changed", "changed", "changed"
				}
				return err
			})
			preparer.generateLeaseOwner = func() (string, error) { return "new-worker", nil }
			preparer.generateObjectKey = func(userID uint64) (string, error) {
				if userID != actor.UserID {
					t.Error("对象键用户范围错误")
				}
				return "users/" + strconv.FormatUint(userID, 10) + "/objects/new-attempt", nil
			}
			// 2. 入口只进入一次事务，核对分支顺序与已提交结果。
			result, err := preparer.Prepare(context.Background(), actor, original.ID)
			if err != nil || result == nil {
				t.Fatal("接收准备失败", err)
			}
			wantID, wantEvents := oldID, "lock,claim,read"
			if original.LeaseVersion > 0 {
				wantID, wantEvents = transaction.newObjectID, "lock,claim,read,create,replace,orphan"
				if transaction.objects[oldID].Status != domain.StoredObjectStatusOrphaned || transaction.input.SizeBytes != original.FileSize {
					t.Fatal("重试对象切换或声明错误")
				}
			}
			if calls != 1 || strings.Join(transaction.events, ",") != wantEvents || result.Task.StagedObjectID != wantID || result.Object.ID != wantID ||
				result.Task.UserID != actor.UserID || result.Task.DeviceID != original.DeviceID || result.Task.LeaseVersion != original.LeaseVersion+1 ||
				*result.Task.LeaseOwner != "new-worker" || result.Task.RequestedName != original.RequestedName || result.Object.Status != domain.StoredObjectStatusStaging {
				t.Fatal("接收准备调用顺序、范围或结果错误")
			}
			for _, used := range transaction.contexts {
				if used != txContext {
					t.Fatal("准备操作未使用同一事务上下文")
				}
			}
			if result.Task == transaction.task || result.Object == transaction.objects[wantID] ||
				(result.Task.ExpectedSHA256 != nil && result.Task.ExpectedSHA256 == transaction.task.ExpectedSHA256) {
				t.Fatal("准备结果未独立持有记录")
			}
		})
	}
}

func TestUploadContentPreparationServiceFailures(t *testing.T) {
	fault := errors.New("private preparation failure")
	for _, phase := range []string{"lock", "claim", "read", "create", "replace", "orphan", "owner generator", "key generator", "outer transaction", "nil lock", "nil claim", "nil read", "nil create", "nil replace", "nil orphan", "bad scope", "bad claim", "bad current object", "first written object", "bad created object", "same key", "bad replaced task", "bad orphan", "empty owner"} {
		t.Run(phase, func(t *testing.T) {
			// 1. 在各阶段注入明确错误或不一致的成功记录。
			actor, transaction := uploadContentPreparationFixture()
			transaction.task.LeaseVersion = 3
			wantError := fault
			switch phase {
			case "owner generator", "key generator", "outer transaction":
			case "nil lock", "nil claim", "nil read", "nil create", "nil replace", "nil orphan":
				transaction.nilStage = strings.TrimPrefix(phase, "nil ")
				wantError = domain.ErrInvalidUploadTaskData
				if transaction.nilStage == "read" || transaction.nilStage == "create" || transaction.nilStage == "orphan" {
					wantError = domain.ErrInvalidStoredObjectData
				}
			case "bad scope", "bad claim", "bad replaced task", "empty owner":
				wantError = domain.ErrInvalidUploadTaskData
			case "bad current object", "first written object", "bad created object", "same key", "bad orphan":
				wantError = domain.ErrInvalidStoredObjectData
			default:
				transaction.failureStage, transaction.failure = phase, fault
			}
			transaction.after = func(stage string, task *domain.UploadTask, object *domain.StoredObject) {
				switch {
				case phase == "bad scope" && stage == "lock":
					task.UserID++
				case phase == "bad claim" && stage == "claim":
					task.StagedObjectID++
				case phase == "bad current object" && stage == "read":
					object.OwnerUserID++
				case phase == "bad created object" && stage == "create":
					object.Provider = "other"
				case phase == "bad replaced task" && stage == "replace":
					task.LeaseVersion++
				case phase == "bad orphan" && stage == "orphan":
					object.Status = domain.StoredObjectStatusStaging
				}
			}
			if phase == "first written object" {
				transaction.task.LeaseVersion = 0
				etag := "written"
				transaction.objects[23].StorageETag = &etag
			}
			preparer := NewUploadContentPreparationService("MINIO", "fixture-bucket", func(ctx context.Context, current domain.Actor, action func(context.Context, UploadContentPreparationTransaction) error) error {
				err := action(ctx, transaction)
				if phase == "outer transaction" {
					return fault
				}
				return err
			})
			preparer.generateLeaseOwner = func() (string, error) {
				if phase == "owner generator" {
					return "", fault
				}
				if phase == "empty owner" {
					return "", nil
				}
				return "new-worker", nil
			}
			preparer.generateObjectKey = func(uint64) (string, error) {
				if phase == "key generator" {
					return "", fault
				}
				if phase == "same key" {
					return "old-attempt", nil
				}
				return "new-attempt", nil
			}
			// 2. 所有失败整体返回空结果，并保留可识别原因。
			result, err := preparer.Prepare(context.Background(), actor, 19)
			if result != nil || !errors.Is(err, wantError) {
				t.Fatal("接收准备失败结果错误", err, transaction.events)
			}
		})
	}
}

func TestUploadContentPreparationServiceCancellation(t *testing.T) {
	for _, phase := range []string{"lock", "claim", "read", "create", "replace", "orphan", "owner generator", "key generator", "after callback"} {
		t.Run(phase, func(t *testing.T) {
			actor, transaction := uploadContentPreparationFixture()
			transaction.task.LeaseVersion = 3
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transaction.after = func(stage string, _ *domain.UploadTask, _ *domain.StoredObject) {
				if stage == phase {
					cancel()
				}
			}
			preparer := NewUploadContentPreparationService("MINIO", "fixture-bucket", func(ctx context.Context, actor domain.Actor, action func(context.Context, UploadContentPreparationTransaction) error) error {
				err := action(ctx, transaction)
				if phase == "after callback" {
					cancel()
				}
				return err
			})
			preparer.generateLeaseOwner = func() (string, error) {
				if phase == "owner generator" {
					cancel()
				}
				return "new-worker", nil
			}
			preparer.generateObjectKey = func(uint64) (string, error) {
				if phase == "key generator" {
					cancel()
				}
				return "new-attempt", nil
			}
			if result, err := preparer.Prepare(ctx, actor, 19); result != nil || !errors.Is(err, context.Canceled) {
				t.Fatal("准备取消未交付空结果和原始原因", err)
			}
		})
	}
}

func TestUploadContentPreparationServiceBoundary(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "zero task", "cancelled", "nil callback context", "nil callback store", "callback skipped"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction := uploadContentPreparationFixture()
			ctx := context.Background()
			taskID := uint64(19)
			calls := 0
			switch mode {
			case "nil context":
				ctx = nil
			case "zero user":
				actor.UserID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero session":
				actor.SessionID = 0
			case "zero task":
				taskID = 0
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			preparer := NewUploadContentPreparationService("MINIO", "fixture-bucket", func(ctx context.Context, actor domain.Actor, action func(context.Context, UploadContentPreparationTransaction) error) error {
				calls++
				switch mode {
				case "nil callback context":
					return action(nil, transaction)
				case "nil callback store":
					return action(ctx, nil)
				case "callback skipped":
					return nil
				}
				return action(ctx, transaction)
			})
			if result, err := preparer.Prepare(ctx, actor, taskID); result != nil || err == nil {
				t.Fatal("无效准备调用未拒绝", err)
			}
			if !strings.HasPrefix(mode, "nil callback") && mode != "callback skipped" && calls != 0 {
				t.Fatal("错误输入仍进入事务")
			}
		})
	}
}

func TestNewUploadContentPreparationService(t *testing.T) {
	for _, mode := range []string{"transaction", "provider", "bucket", "long bucket", "non ASCII provider"} {
		t.Run(mode, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("无效装配未拒绝")
				}
			}()
			provider, bucket := "MINIO", "fixture-bucket"
			withTx := func(context.Context, domain.Actor, func(context.Context, UploadContentPreparationTransaction) error) error {
				return nil
			}
			switch mode {
			case "transaction":
				withTx = nil
			case "provider":
				provider = ""
			case "bucket":
				bucket = ""
			case "long bucket":
				bucket = strings.Repeat("a", 64)
			case "non ASCII provider":
				provider = "对象存储"
			}
			NewUploadContentPreparationService(provider, bucket, withTx)
		})
	}
}

func TestUploadContentPreparationServiceRandomOwner(t *testing.T) {
	seen := make(map[string]bool)
	for range 2 {
		actor, transaction := uploadContentPreparationFixture()
		preparer := NewUploadContentPreparationService("MINIO", "fixture-bucket", func(ctx context.Context, actor domain.Actor, action func(context.Context, UploadContentPreparationTransaction) error) error {
			return action(ctx, transaction)
		})
		result, err := preparer.Prepare(context.Background(), actor, 19)
		if err != nil {
			t.Fatal(err)
		}
		owner := *result.Task.LeaseOwner
		identifier, err := uuid.Parse(owner)
		if err != nil || identifier.Version() != 4 || identifier.Variant() != uuid.RFC4122 || identifier.String() != owner || seen[owner] {
			t.Fatal("接收持有者未生成独立规范 UUID")
		}
		seen[owner] = true
	}
	// 标准随机源错误经真实默认生成器传播。
	fault := errors.New("private random source failure")
	uuid.SetRand(iotest.ErrReader(fault))
	defer uuid.SetRand(nil)
	actor, transaction := uploadContentPreparationFixture()
	preparer := NewUploadContentPreparationService("MINIO", "fixture-bucket", func(ctx context.Context, actor domain.Actor, action func(context.Context, UploadContentPreparationTransaction) error) error {
		return action(ctx, transaction)
	})
	if result, err := preparer.Prepare(context.Background(), actor, 19); result != nil || !errors.Is(err, fault) || !reflect.DeepEqual(transaction.events, []string{"lock"}) {
		t.Fatal("随机源故障仍领取租约或丢失原因", err)
	}
}
