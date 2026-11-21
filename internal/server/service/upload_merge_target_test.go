package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadMergeTargetGet(t *testing.T) {
	const provider, bucket = "local_minio", "fixture-bucket"
	const objectKey, uploadID = "users/7/objects/merge +%.bin", "Opaque+A/= "
	now := time.Date(2026, 11, 22, 12, 0, 0, 123456000, time.UTC)
	taskOnly := []string{"task"}
	taskAndObject := []string{"task", "object"}
	fullRead := []string{"task", "object", "task"}
	readFailure := errors.New("fixture read failure")
	for _, fixture := range []struct {
		name                      string
		changeClaim, changeTask   func(*domain.UploadTask)
		changeFinalTask           func(*domain.UploadTask)
		changeObject              func(*domain.StoredObject)
		firstElapsed, lastElapsed time.Duration
		objectError, finalError   error
		cancelOnObjectRead        bool
		want                      error
		wantReads                 []string
	}{
		{name: "valid bound target", wantReads: fullRead},
		{name: "still valid one microsecond before deadline", lastElapsed: time.Minute - time.Microsecond, wantReads: fullRead},
		{name: "claim without owner", changeClaim: func(task *domain.UploadTask) { task.LeaseOwner = nil }, want: domain.ErrInvalidUploadTaskData},
		{name: "claim at wrong stage", changeClaim: func(task *domain.UploadTask) { task.Status = domain.UploadStatusUploading }, want: domain.ErrUploadTaskNotReady, wantReads: taskOnly},
		{name: "different returned account", changeTask: func(task *domain.UploadTask) { task.UserID++ }, want: domain.ErrInvalidUploadTaskData, wantReads: taskOnly},
		{name: "lease already expired", firstElapsed: time.Minute, want: domain.ErrUploadTaskBusy, wantReads: taskOnly},
		{name: "session changed case only", changeTask: func(task *domain.UploadTask) { task.StorageUploadID = new("opaque+A/= ") }, want: domain.ErrUploadTaskBusy, wantReads: taskOnly},
		{name: "session missing", changeTask: func(task *domain.UploadTask) { task.StorageUploadID = nil }, want: domain.ErrUploadTaskNotReady, wantReads: taskOnly},
		{name: "changed staged object", changeTask: func(task *domain.UploadTask) { task.StagedObjectID++ }, want: domain.ErrUploadTaskBusy, wantReads: taskOnly},
		{name: "wrong object owner", changeObject: func(object *domain.StoredObject) { object.OwnerUserID++ }, want: domain.ErrInvalidStoredObjectData, wantReads: taskAndObject},
		{name: "wrong object ID", changeObject: func(object *domain.StoredObject) { object.ID++ }, want: domain.ErrInvalidStoredObjectData, wantReads: taskAndObject},
		{name: "wrong object size", changeObject: func(object *domain.StoredObject) { object.FileSize++ }, want: domain.ErrInvalidStoredObjectData, wantReads: taskAndObject},
		{name: "wrong bucket", changeObject: func(object *domain.StoredObject) { object.Bucket += "-other" }, want: domain.ErrInvalidStoredObjectData, wantReads: taskAndObject},
		{name: "wrong provider", changeObject: func(object *domain.StoredObject) { object.Provider += "-other" }, want: domain.ErrInvalidStoredObjectData, wantReads: taskAndObject},
		{name: "invalid key", changeObject: func(object *domain.StoredObject) { object.ObjectKey += "\n" }, want: domain.ErrInvalidStoredObjectData, wantReads: taskAndObject},
		{name: "not staged", changeObject: func(object *domain.StoredObject) { object.Status = domain.StoredObjectStatusVerifying }, want: domain.ErrInvalidStoredObjectData, wantReads: taskAndObject},
		{name: "object result already recorded", changeObject: func(object *domain.StoredObject) { object.StorageVersionID = new("fixture-completed") }, want: domain.ErrInvalidStoredObjectData, wantReads: taskAndObject},
		{name: "object missing", objectError: domain.ErrStoredObjectNotFound, want: domain.ErrStoredObjectNotFound, wantReads: taskAndObject},
		{name: "lease expires during object read", lastElapsed: time.Minute, want: domain.ErrUploadTaskBusy, wantReads: fullRead},
		{name: "task expires during object read", lastElapsed: time.Hour, want: domain.ErrUploadTaskNotReady, wantReads: fullRead},
		{name: "newer lease with same owner", changeFinalTask: func(task *domain.UploadTask) { task.LeaseVersion++ }, want: domain.ErrUploadTaskBusy, wantReads: fullRead},
		{name: "lease owner changed case only", changeFinalTask: func(task *domain.UploadTask) { task.LeaseOwner = new("Merge-worker") }, want: domain.ErrUploadTaskBusy, wantReads: fullRead},
		{name: "lease deadline changed", changeFinalTask: func(task *domain.UploadTask) { task.LeaseUntil = new(task.LeaseUntil.Add(time.Second)) }, want: domain.ErrUploadTaskBusy, wantReads: fullRead},
		{name: "task returned to uploading", changeFinalTask: func(task *domain.UploadTask) { task.Status = domain.UploadStatusUploading }, want: domain.ErrUploadTaskNotReady, wantReads: fullRead},
		{name: "session loses trailing space", changeFinalTask: func(task *domain.UploadTask) { task.StorageUploadID = new("Opaque+A/=") }, want: domain.ErrUploadTaskBusy, wantReads: fullRead},
		{name: "declared digest changed", changeFinalTask: func(task *domain.UploadTask) { task.ExpectedSHA256[0] ^= 1 }, want: domain.ErrUploadTaskNotReady, wantReads: fullRead},
		{name: "database failed on recheck", finalError: readFailure, want: readFailure, wantReads: fullRead},
		{name: "canceled as object read returns", cancelOnObjectRead: true, want: context.Canceled, wantReads: taskAndObject},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 独立准备领取结果、两次任务读回及暂存对象。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			claimed := uploadPartSigningTaskFixture(now.Add(time.Hour))
			claimed.Status, claimed.LeaseOwner, claimed.LeaseUntil = domain.UploadStatusCompleting, new("merge-worker"), new(now.Add(time.Minute))
			claimed.StorageUploadID = new(uploadID)
			current := copyUploadTask(claimed)
			final := copyUploadTask(current)
			object := &domain.StoredObject{ID: 23, OwnerUserID: 7, Provider: provider, Bucket: bucket,
				ObjectKey: objectKey, FileSize: claimed.FileSize, Status: domain.StoredObjectStatusStaging}
			if fixture.changeClaim != nil {
				fixture.changeClaim(claimed)
			}
			if fixture.changeTask != nil {
				fixture.changeTask(current)
			}
			if fixture.changeFinalTask != nil {
				fixture.changeFinalTask(final)
			}
			if fixture.changeObject != nil {
				fixture.changeObject(object)
			}

			// 2. 用隔离查询夹具模拟替换租约或对象，并核对所有读取的账号范围。
			var reads []string
			targets := NewUploadPartSigningTargetService(provider, bucket,
				func(queryContext context.Context, userID, taskID uint64) (*domain.UploadTask, time.Time, error) {
					reads = append(reads, "task")
					if queryContext != ctx || userID != 7 || taskID != 19 {
						t.Fatal("未按可信账号及任务 ID 读取")
					}
					if len(reads) == 1 {
						return current, now.Add(fixture.firstElapsed), nil
					}
					return final, now.Add(fixture.lastElapsed), fixture.finalError
				},
				func(queryContext context.Context, userID, objectID uint64) (*domain.StoredObject, error) {
					reads = append(reads, "object")
					if queryContext != ctx || userID != 7 || objectID != 23 {
						t.Fatal("未按已核对的账号和对象 ID 读取")
					}
					if fixture.cancelOnObjectRead {
						cancel()
					}
					return object, fixture.objectError
				})

			// 3. 检查复核顺序和失败时的空结果，成功时保留原对象键和会话。
			target, err := targets.GetForMerge(ctx, claimed)
			if !errors.Is(err, fixture.want) || !reflect.DeepEqual(reads, fixture.wantReads) {
				t.Fatalf("读取顺序或结果有误，得到 %v、%v，预期 %v、%v", reads, err, fixture.wantReads, fixture.want)
			}
			if fixture.want != nil {
				if target != nil {
					t.Fatal("读取失败仍交付了存储目标")
				}
				return
			}
			want := UploadPartSigningTarget{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 2, ObjectKey: objectKey,
				UploadID: uploadID, FileSizeBytes: 16777217, PartSizeBytes: 16777216, PartCount: 2, ExpiresAt: now.Add(time.Hour)}
			*claimed.StorageUploadID, *current.StorageUploadID, *final.StorageUploadID, object.ObjectKey = "changed", "changed", "changed", "changed"
			if target == nil || *target != want {
				t.Fatal("目标未独立保留原对象键、会话与分片计划")
			}
		})
	}
}
