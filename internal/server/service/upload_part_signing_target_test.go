package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadPartSigningTargetGet(t *testing.T) {
	const provider, bucket = "local_minio", "fixture-bucket"
	const key = "users/7/objects/fixture +%.bin"
	now := time.Date(2026, 11, 21, 12, 0, 0, 123456000, time.UTC)
	actor := domain.Actor{UserID: 7, DeviceID: 31, SessionID: 41}
	taskFault := errors.New("fixture task query failure")
	objectFault := errors.New("fixture object query failure")
	for _, test := range []struct {
		name         string
		changeTask   func(*domain.UploadTask)
		changeObject func(*domain.StoredObject)
		taskError    error
		objectError  error
		cancelAt     string
		want         error
		wantReads    []string
		missing      bool
	}{
		{name: "same account different device", wantReads: []string{"task", "object"}},
		{name: "missing task", taskError: domain.ErrUploadTaskNotFound, want: domain.ErrUploadTaskNotFound, wantReads: []string{"task"}},
		{name: "task lookup failed", taskError: taskFault, want: taskFault, wantReads: []string{"task"}},
		{name: "cancelled before read", cancelAt: "before", want: context.Canceled},
		{name: "cancelled during task lookup", cancelAt: "task", want: context.Canceled, wantReads: []string{"task"}},
		{name: "cancelled during object lookup", cancelAt: "object", want: context.Canceled, wantReads: []string{"task", "object"}},
		{name: "other account task", changeTask: func(task *domain.UploadTask) { task.UserID++ }, want: domain.ErrInvalidUploadTaskData, wantReads: []string{"task"}},
		{name: "task not ready", changeTask: func(task *domain.UploadTask) { task.Status = domain.UploadStatusCreating }, want: domain.ErrUploadTaskNotReady, wantReads: []string{"task"}},
		{name: "active lease", changeTask: func(task *domain.UploadTask) {
			task.LeaseOwner, task.LeaseUntil = new("fixture-owner"), new(now.Add(time.Second))
		}, want: domain.ErrUploadTaskBusy, wantReads: []string{"task"}},
		{name: "missing object", objectError: domain.ErrStoredObjectNotFound, want: domain.ErrStoredObjectNotFound, wantReads: []string{"task", "object"}},
		{name: "object lookup failed", objectError: objectFault, want: objectFault, wantReads: []string{"task", "object"}},
		{name: "nil object", missing: true, want: domain.ErrInvalidStoredObjectData, wantReads: []string{"task", "object"}},
		{name: "other object", changeObject: func(object *domain.StoredObject) { object.ID++ }, want: domain.ErrInvalidStoredObjectData, wantReads: []string{"task", "object"}},
		{name: "other object owner", changeObject: func(object *domain.StoredObject) { object.OwnerUserID++ }, want: domain.ErrInvalidStoredObjectData, wantReads: []string{"task", "object"}},
		{name: "other provider", changeObject: func(object *domain.StoredObject) { object.Provider = "fixture-other" }, want: domain.ErrInvalidStoredObjectData, wantReads: []string{"task", "object"}},
		{name: "other bucket", changeObject: func(object *domain.StoredObject) { object.Bucket = "other-bucket" }, want: domain.ErrInvalidStoredObjectData, wantReads: []string{"task", "object"}},
		{name: "empty key", changeObject: func(object *domain.StoredObject) { object.ObjectKey = " " }, want: domain.ErrInvalidStoredObjectData, wantReads: []string{"task", "object"}},
		{name: "key with control character", changeObject: func(object *domain.StoredObject) { object.ObjectKey += "\n" }, want: domain.ErrInvalidStoredObjectData, wantReads: []string{"task", "object"}},
		{name: "oversized key", changeObject: func(object *domain.StoredObject) { object.ObjectKey = strings.Repeat("a", 256) }, want: domain.ErrInvalidStoredObjectData, wantReads: []string{"task", "object"}},
		{name: "wrong size", changeObject: func(object *domain.StoredObject) { object.FileSize-- }, want: domain.ErrInvalidStoredObjectData, wantReads: []string{"task", "object"}},
		{name: "no longer staging", changeObject: func(object *domain.StoredObject) { object.Status = domain.StoredObjectStatusVerifying }, want: domain.ErrInvalidStoredObjectData, wantReads: []string{"task", "object"}},
		{name: "already completed", changeObject: func(object *domain.StoredObject) { object.StorageVersionID = new("completed-version") }, want: domain.ErrInvalidStoredObjectData, wantReads: []string{"task", "object"}},
		{name: "scheduled for cleanup", changeObject: func(object *domain.StoredObject) { object.GCAfter = &now }, want: domain.ErrInvalidStoredObjectData, wantReads: []string{"task", "object"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancelAt == "before" {
				cancel()
			}
			task := uploadPartSigningTaskFixture(now.Add(time.Hour))
			object := &domain.StoredObject{ID: task.StagedObjectID, OwnerUserID: actor.UserID, Provider: provider,
				Bucket: bucket, ObjectKey: key, FileSize: task.FileSize, Status: domain.StoredObjectStatusStaging}
			if test.changeTask != nil {
				test.changeTask(task)
			}
			if test.changeObject != nil {
				test.changeObject(object)
			}
			var reads []string
			targets := NewUploadPartSigningTargetService(provider, bucket,
				func(queryContext context.Context, userID, taskID uint64) (*domain.UploadTask, time.Time, error) {
					reads = append(reads, "task")
					if queryContext != ctx || userID != 7 || taskID != 19 {
						t.Fatal("任务查询未按认证账号和请求任务 ID 读取")
					}
					if test.cancelAt == "task" {
						cancel()
					}
					return task, now, test.taskError
				},
				func(queryContext context.Context, userID, objectID uint64) (*domain.StoredObject, error) {
					reads = append(reads, "object")
					if queryContext != ctx || userID != 7 || objectID != 23 {
						t.Fatal("对象查询未使用相同账号和任务引用")
					}
					if test.cancelAt == "object" {
						cancel()
					}
					if test.missing {
						return nil, nil
					}
					return object, test.objectError
				},
			)
			result, err := targets.Get(ctx, actor, 19)
			if !errors.Is(err, test.want) || !reflect.DeepEqual(reads, test.wantReads) {
				t.Fatalf("调用顺序或拒绝原因不符，得到 %v %v，期望 %v %v", reads, err, test.wantReads, test.want)
			}
			if test.want != nil {
				if result != nil {
					t.Fatal("失败时返回了签名目标")
				}
				return
			}
			want := UploadPartSigningTarget{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 2,
				ObjectKey: key, UploadID: "opaque+A/=", FileSizeBytes: 16777217, PartSizeBytes: 16777216, PartCount: 2, ExpiresAt: now.Add(time.Hour)}
			*task.StorageUploadID, *task.PartSize, object.ObjectKey = "changed", 0, "other"
			if result == nil || *result != want {
				t.Fatal("签名目标未准确保留独立的已校验数据", result)
			}
		})
	}
}

func TestUploadPartReceiptTargetGet(t *testing.T) {
	const provider, bucket = "local_minio", "fixture-bucket"
	const key = "users/7/objects/receipt-fixture"
	now := time.Date(2026, 11, 21, 12, 0, 0, 123456000, time.UTC)
	actor := domain.Actor{UserID: 7, DeviceID: 31, SessionID: 41}
	queryFault := errors.New("fixture object lookup failure")
	for _, test := range []struct {
		name         string
		taskError    error
		objectError  error
		changeObject func(*domain.StoredObject)
		cancel       bool
		want         error
	}{
		{name: "active lease and same account different device"},
		{name: "task outside scope", taskError: domain.ErrUploadTaskNotFound, want: domain.ErrUploadTaskNotFound},
		{name: "missing stored object", objectError: domain.ErrStoredObjectNotFound, want: domain.ErrUploadTaskNotReady},
		{name: "other account object", changeObject: func(object *domain.StoredObject) { object.OwnerUserID++ }, want: domain.ErrUploadTaskNotReady},
		{name: "location changed", changeObject: func(object *domain.StoredObject) { object.Bucket = "other-bucket" }, want: domain.ErrUploadTaskNotReady},
		{name: "no longer staging", changeObject: func(object *domain.StoredObject) { object.Status = domain.StoredObjectStatusVerifying }, want: domain.ErrUploadTaskNotReady},
		{name: "database failure", objectError: queryFault, want: queryFault},
		{name: "cancel overrides object error", objectError: domain.ErrStoredObjectNotFound, cancel: true, want: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			task := uploadPartSigningTaskFixture(now.Add(time.Hour))
			task.LeaseOwner, task.LeaseUntil = new("fixture-completer"), new(now.Add(time.Minute))
			object := &domain.StoredObject{ID: task.StagedObjectID, OwnerUserID: actor.UserID, Provider: provider,
				Bucket: bucket, ObjectKey: key, FileSize: task.FileSize, Status: domain.StoredObjectStatusStaging}
			if test.changeObject != nil {
				test.changeObject(object)
			}
			var reads []string
			targets := NewUploadPartSigningTargetService(provider, bucket,
				func(queryContext context.Context, userID, taskID uint64) (*domain.UploadTask, time.Time, error) {
					reads = append(reads, "task")
					if queryContext != ctx || userID != actor.UserID || taskID != task.ID {
						t.Fatal("任务读取没有使用认证账号和请求任务")
					}
					return task, now, test.taskError
				},
				func(queryContext context.Context, userID, objectID uint64) (*domain.StoredObject, error) {
					reads = append(reads, "object")
					if queryContext != ctx || userID != actor.UserID || objectID != task.StagedObjectID {
						t.Fatal("对象读取没有使用同一账号和任务的对象 ID")
					}
					if test.cancel {
						cancel()
					}
					return object, test.objectError
				},
			)
			target, err := targets.GetForReceipts(ctx, actor, task.ID)
			wantReads := []string{"task", "object"}
			if test.taskError != nil {
				wantReads = wantReads[:1]
			}
			if !errors.Is(err, test.want) || !reflect.DeepEqual(reads, wantReads) {
				t.Fatal("只读目标查询顺序或错误类别不符", reads, err)
			}
			if test.want != nil {
				if target != nil {
					t.Fatal("读取失败后仍交付了目标")
				}
				return
			}
			want := UploadPartSigningTarget{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 2,
				ObjectKey: key, UploadID: "opaque+A/=", FileSizeBytes: 16777217, PartSizeBytes: 16777216, PartCount: 2, ExpiresAt: now.Add(time.Hour)}
			*task.StorageUploadID, *task.PartSize, object.ObjectKey = "new-session", 0, "new-key"
			if target == nil || *target != want {
				t.Fatal("回执目标未独立保存已核对的会话、位置和计划")
			}
		})
	}
}
