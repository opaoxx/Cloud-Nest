package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadCompletionTargetGet(t *testing.T) {
	const provider, bucket = "local_minio", "fixture-bucket"
	const objectKey = "users/7/objects/completion +%.bin"
	now := time.Date(2026, 11, 22, 12, 0, 0, 123456000, time.UTC)
	actor := domain.Actor{UserID: 7, DeviceID: 31, SessionID: 41}
	taskOnly := []string{"task"}
	taskAndObject := []string{"task", "object"}
	objectFault := errors.New("fixture object read failure")
	for _, test := range []struct {
		name                    string
		changeClaim, changeTask func(*domain.UploadTask)
		changeObject            func(*domain.StoredObject)
		taskError, objectError  error
		elapsed                 time.Duration
		cancelAt                string
		want                    error
		wantReads               []string
	}{
		{name: "owner from another device of same account", wantReads: taskAndObject},
		{name: "one microsecond of lease remaining", elapsed: time.Minute - time.Microsecond, wantReads: taskAndObject},
		{name: "unowned claim", changeClaim: func(task *domain.UploadTask) { task.UserID++ }, want: domain.ErrUploadTaskNotFound},
		{name: "claim without owner", changeClaim: func(task *domain.UploadTask) { task.LeaseOwner = nil }, want: domain.ErrInvalidUploadTaskData},
		{name: "task outside scope", taskError: domain.ErrUploadTaskNotFound, want: domain.ErrUploadTaskNotFound, wantReads: taskOnly},
		{name: "different returned user", changeTask: func(task *domain.UploadTask) { task.UserID++ }, want: domain.ErrInvalidUploadTaskData, wantReads: taskOnly},
		{name: "no longer uploading", changeTask: func(task *domain.UploadTask) { task.Status = domain.UploadStatusCompleting }, want: domain.ErrUploadTaskNotReady, wantReads: taskOnly},
		{name: "task reaches deadline", elapsed: time.Hour, want: domain.ErrUploadTaskNotReady, wantReads: taskOnly},
		{name: "lease reaches deadline", elapsed: time.Minute, want: domain.ErrUploadTaskBusy, wantReads: taskOnly},
		{name: "released lease", changeTask: func(task *domain.UploadTask) { task.LeaseOwner, task.LeaseUntil = nil, nil }, want: domain.ErrUploadTaskBusy, wantReads: taskOnly},
		{name: "same owner newer version", changeTask: func(task *domain.UploadTask) { task.LeaseVersion++ }, want: domain.ErrUploadTaskBusy, wantReads: taskOnly},
		{name: "different owner", changeTask: func(task *domain.UploadTask) { task.LeaseOwner = new("Completion-owner") }, want: domain.ErrUploadTaskBusy, wantReads: taskOnly},
		{name: "different lease deadline", changeTask: func(task *domain.UploadTask) { task.LeaseUntil = new(now.Add(2 * time.Minute)) }, want: domain.ErrUploadTaskBusy, wantReads: taskOnly},
		{name: "changed staging object", changeTask: func(task *domain.UploadTask) { task.StagedObjectID++ }, want: domain.ErrUploadTaskBusy, wantReads: taskOnly},
		{name: "session changed case only", changeTask: func(task *domain.UploadTask) { task.StorageUploadID = new("Opaque+A/=") }, want: domain.ErrUploadTaskBusy, wantReads: taskOnly},
		{name: "different valid plan", changeTask: func(task *domain.UploadTask) { task.FileSize++; task.ReservedBytes++ }, want: domain.ErrUploadTaskNotReady, wantReads: taskOnly},
		{name: "different file checksum", changeTask: func(task *domain.UploadTask) { task.ExpectedSHA256[0] ^= 1 }, want: domain.ErrUploadTaskNotReady, wantReads: taskOnly},
		{name: "missing staged object", objectError: domain.ErrStoredObjectNotFound, want: domain.ErrUploadTaskNotReady, wantReads: taskAndObject},
		{name: "wrong object owner", changeObject: func(object *domain.StoredObject) { object.OwnerUserID++ }, want: domain.ErrUploadTaskNotReady, wantReads: taskAndObject},
		{name: "wrong bucket", changeObject: func(object *domain.StoredObject) { object.Bucket += "-other" }, want: domain.ErrUploadTaskNotReady, wantReads: taskAndObject},
		{name: "wrong provider", changeObject: func(object *domain.StoredObject) { object.Provider += "-other" }, want: domain.ErrUploadTaskNotReady, wantReads: taskAndObject},
		{name: "invalid key", changeObject: func(object *domain.StoredObject) { object.ObjectKey += "\n" }, want: domain.ErrUploadTaskNotReady, wantReads: taskAndObject},
		{name: "no longer staged", changeObject: func(object *domain.StoredObject) { object.Status = domain.StoredObjectStatusVerifying }, want: domain.ErrUploadTaskNotReady, wantReads: taskAndObject},
		{name: "object lookup unavailable", objectError: objectFault, want: objectFault, wantReads: taskAndObject},
		{name: "cancel before access", cancelAt: "before", want: context.Canceled},
		{name: "cancel as task returns", cancelAt: "task", want: context.Canceled, wantReads: taskOnly},
		{name: "cancel as object returns", cancelAt: "object", want: context.Canceled, wantReads: taskAndObject},
	} {
		t.Run(test.name, func(t *testing.T) {
			// 1. 构造互相独立的领取结果和数据库现态。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			claimed := uploadPartSigningTaskFixture(now.Add(time.Hour))
			claimed.LeaseOwner, claimed.LeaseUntil = new("completion-owner"), new(now.Add(time.Minute))
			current := copyUploadTask(claimed)
			object := &domain.StoredObject{ID: claimed.StagedObjectID, OwnerUserID: actor.UserID, Provider: provider,
				Bucket: bucket, ObjectKey: objectKey, FileSize: claimed.FileSize, Status: domain.StoredObjectStatusStaging}
			if test.changeClaim != nil {
				test.changeClaim(claimed)
			}
			if test.changeTask != nil {
				test.changeTask(current)
			}
			if test.changeObject != nil {
				test.changeObject(object)
			}
			if test.cancelAt == "before" {
				cancel()
			}
			var reads []string
			targets := NewUploadPartSigningTargetService(provider, bucket,
				func(queryContext context.Context, userID, taskID uint64) (*domain.UploadTask, time.Time, error) {
					reads = append(reads, "task")
					if queryContext != ctx || userID != actor.UserID || taskID != 19 {
						t.Fatal("任务查询未限制在可信账号及目标任务")
					}
					if test.cancelAt == "task" {
						cancel()
					}
					return current, now.Add(test.elapsed), test.taskError
				},
				func(queryContext context.Context, userID, objectID uint64) (*domain.StoredObject, error) {
					reads = append(reads, "object")
					if queryContext != ctx || userID != actor.UserID || objectID != 23 {
						t.Fatal("对象读取未复用已核对的账号与对象 ID")
					}
					if test.cancelAt == "object" {
						cancel()
					}
					return object, test.objectError
				})

			// 2. 核对拒绝时机，成功结果独立保留原会话和存储定位。
			target, err := targets.GetForCompletion(ctx, actor, claimed)
			if !errors.Is(err, test.want) || !reflect.DeepEqual(reads, test.wantReads) {
				t.Fatalf("读取顺序或结果有误，得到 %v、%v，预期 %v、%v", reads, err, test.wantReads, test.want)
			}
			if test.want != nil {
				if target != nil {
					t.Fatal("读取失败仍交付了完成目标")
				}
				return
			}
			want := UploadPartSigningTarget{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 2, ObjectKey: objectKey,
				UploadID: "opaque+A/=", FileSizeBytes: 16777217, PartSizeBytes: 16777216, PartCount: 2, ExpiresAt: now.Add(time.Hour)}
			*claimed.StorageUploadID, *current.StorageUploadID, object.ObjectKey = "changed", "changed", "changed"
			if target == nil || *target != want {
				t.Fatal("目标未独立保留原会话、键与固定计划")
			}
		})
	}
}
