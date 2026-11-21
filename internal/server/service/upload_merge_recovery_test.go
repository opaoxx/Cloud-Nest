package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadMergeRecoveryServiceCheck(t *testing.T) {
	const provider, bucket = "local_minio", "fixture-bucket"
	const key, session = "users/7/objects/merge +%.bin", "Opaque+A/= "
	const etag, version = "Fixture-Object-2", "Version+A/="
	now := time.Date(2026, 11, 22, 12, 0, 0, 123456000, time.UTC)
	fullFlow := []string{"target", "head", "target"}
	for _, fixture := range []struct {
		name                  string
		before, after         time.Duration
		changeClaim           func(*domain.UploadTask)
		changeAfterTask       func(*domain.UploadTask)
		changeMetadata        func(*domain.UploadObjectResult) *domain.UploadObjectResult
		changeKey, cancelHEAD bool
		statError, want       error
		wantCalls             []string
	}{
		{name: "matched version and size", wantCalls: fullFlow},
		{name: "one microsecond before lease deadline", after: time.Minute - time.Microsecond, wantCalls: fullFlow},
		{name: "unversioned result", changeMetadata: func(result *domain.UploadObjectResult) *domain.UploadObjectResult {
			result.StorageVersionID = nil
			return result
		}, wantCalls: fullFlow},
		{name: "explicit null version", changeMetadata: func(result *domain.UploadObjectResult) *domain.UploadObjectResult {
			result.StorageVersionID = new("null")
			return result
		}, wantCalls: fullFlow},
		{name: "invalid claim stage", changeClaim: func(task *domain.UploadTask) { task.Status = domain.UploadStatusUploading }, want: domain.ErrInvalidUploadTaskData},
		{name: "lease already expired", before: time.Minute, want: domain.ErrUploadTaskBusy, wantCalls: fullFlow[:1]},
		{name: "object missing", statError: domain.ErrUploadObjectMissing, want: domain.ErrUploadObjectMissing, wantCalls: fullFlow},
		{name: "storage unavailable", statError: domain.ErrUploadStorageUnavailable, want: domain.ErrUploadStorageUnavailable, wantCalls: fullFlow},
		{name: "HEAD deadline shorter than caller", statError: context.DeadlineExceeded, want: context.DeadlineExceeded, wantCalls: fullFlow},
		{name: "short size", changeMetadata: func(result *domain.UploadObjectResult) *domain.UploadObjectResult {
			result.SizeBytes--
			return result
		}, want: domain.ErrUploadBodySizeMismatch, wantCalls: fullFlow},
		{name: "long size", changeMetadata: func(result *domain.UploadObjectResult) *domain.UploadObjectResult {
			result.SizeBytes++
			return result
		}, want: domain.ErrUploadBodySizeMismatch, wantCalls: fullFlow},
		{name: "invalid empty response", changeMetadata: func(*domain.UploadObjectResult) *domain.UploadObjectResult { return nil },
			want: domain.ErrUploadStorageUnavailable, wantCalls: fullFlow},
		{name: "missing ETag", changeMetadata: func(result *domain.UploadObjectResult) *domain.UploadObjectResult {
			result.StorageETag = nil
			return result
		}, want: domain.ErrUploadStorageUnavailable, wantCalls: fullFlow},
		{name: "empty stored version", changeMetadata: func(result *domain.UploadObjectResult) *domain.UploadObjectResult {
			result.StorageVersionID = new("")
			return result
		}, want: domain.ErrUploadStorageUnavailable, wantCalls: fullFlow},
		{name: "lease expires during successful HEAD", after: time.Minute, want: domain.ErrUploadTaskBusy, wantCalls: fullFlow},
		{name: "lease expires during missing response", after: time.Minute, statError: domain.ErrUploadObjectMissing, want: domain.ErrUploadTaskBusy, wantCalls: fullFlow},
		{name: "task reaches expiry during HEAD", after: time.Hour, want: domain.ErrUploadTaskNotReady, wantCalls: fullFlow},
		{name: "same owner with a newer lease", changeAfterTask: func(task *domain.UploadTask) { task.LeaseVersion++ }, want: domain.ErrUploadTaskBusy, wantCalls: fullFlow},
		{name: "session changes during HEAD", changeAfterTask: func(task *domain.UploadTask) { task.StorageUploadID = new("Opaque+A/=") }, want: domain.ErrUploadTaskBusy, wantCalls: fullFlow},
		{name: "object key changed", changeKey: true, want: domain.ErrUploadTaskNotReady, wantCalls: fullFlow},
		{name: "canceled during HEAD", cancelHEAD: true, want: context.Canceled, wantCalls: fullFlow[:2]},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 用独立的任务读取结果和数据库时钟模拟一次有效租约。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			claimed := uploadPartSigningTaskFixture(now.Add(time.Hour))
			claimed.Status, claimed.LeaseOwner, claimed.LeaseUntil = domain.UploadStatusCompleting, new("merge-worker"), new(now.Add(time.Minute))
			claimed.StorageUploadID = new(session)
			beforeTask, afterTask := copyUploadTask(claimed), copyUploadTask(claimed)
			if fixture.changeClaim != nil {
				fixture.changeClaim(claimed)
			}
			if fixture.changeAfterTask != nil {
				fixture.changeAfterTask(afterTask)
			}
			originalClaim := copyUploadTask(claimed)
			metadata := &domain.UploadObjectResult{SizeBytes: claimed.FileSize, StorageETag: new(etag), StorageVersionID: new(version)}
			if fixture.changeMetadata != nil {
				metadata = fixture.changeMetadata(metadata)
			}

			// 2. 走实际目标校验入口，夹具仅提供数据库和 HEAD 读回的数据。
			headCalled := false
			targets := NewUploadPartSigningTargetService(provider, bucket,
				func(queryContext context.Context, userID, taskID uint64) (*domain.UploadTask, time.Time, error) {
					if queryContext != ctx || userID != claimed.UserID || taskID != claimed.ID {
						t.Fatal("任务读取没有沿用原账号和任务")
					}
					if headCalled {
						return afterTask, now.Add(fixture.after), nil
					}
					return beforeTask, now.Add(fixture.before), nil
				},
				func(queryContext context.Context, userID, objectID uint64) (*domain.StoredObject, error) {
					if queryContext != ctx || userID != claimed.UserID || objectID != claimed.StagedObjectID {
						t.Fatal("对象读取没有沿用原暂存对象")
					}
					objectKey := key
					if headCalled && fixture.changeKey {
						objectKey += "-different"
					}
					return &domain.StoredObject{ID: objectID, OwnerUserID: userID, Provider: provider,
						Bucket: bucket, ObjectKey: objectKey, FileSize: claimed.FileSize, Status: domain.StoredObjectStatusStaging}, nil
				})
			var calls []string
			recovery := NewUploadMergeRecoveryService(
				func(queryContext context.Context, lease *domain.UploadTask) (*UploadPartSigningTarget, error) {
					calls = append(calls, "target")
					if queryContext != ctx || lease != claimed {
						t.Fatal("目标查询没有使用当前租约")
					}
					return targets.GetForMerge(queryContext, lease)
				},
				func(queryContext context.Context, objectKey string) (*domain.UploadObjectResult, error) {
					calls = append(calls, "head")
					if queryContext != ctx || objectKey != key || headCalled {
						t.Fatal("HEAD 没有使用原对象键或被重复调用")
					}
					headCalled = true
					if fixture.cancelHEAD {
						cancel()
					}
					return metadata, fixture.statError
				})

			// 3. 核对调用顺序、错误类别和目标与元数据的对应关系。
			result, err := recovery.Check(ctx, claimed)
			if !errors.Is(err, fixture.want) || !reflect.DeepEqual(calls, fixture.wantCalls) || !reflect.DeepEqual(claimed, originalClaim) {
				t.Fatalf("恢复检查结果或调用顺序错误，得到 %v、%v，预期 %v、%v", err, calls, fixture.want, fixture.wantCalls)
			}
			if fixture.want != nil {
				if result != nil {
					t.Fatal("失败时仍交付了对象元数据", result)
				}
				return
			}
			wantTarget := UploadPartSigningTarget{TaskID: claimed.ID, UserID: claimed.UserID, StagedObjectID: claimed.StagedObjectID,
				LeaseVersion: claimed.LeaseVersion, ObjectKey: key, UploadID: session, FileSizeBytes: claimed.FileSize,
				PartSizeBytes: *claimed.PartSize, PartCount: *claimed.PartCount, ExpiresAt: claimed.ExpiresAt}
			wantObject := domain.UploadObjectResult{SizeBytes: claimed.FileSize, StorageETag: new(etag), StorageVersionID: copyOptionalValue(metadata.StorageVersionID)}
			*metadata.StorageETag = "different"
			if metadata.StorageVersionID != nil {
				*metadata.StorageVersionID = "different"
			}
			if result == nil || result.Target != wantTarget || !reflect.DeepEqual(result.Object, wantObject) {
				t.Fatal("未独立保留当前目标和可空版本定位", result)
			}
		})
	}
}
