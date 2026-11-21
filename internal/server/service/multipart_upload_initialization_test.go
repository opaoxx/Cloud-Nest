package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartUploadInitializationProcessRetryAndErrors(t *testing.T) {
	const provider, bucket, owner = "local_minio", "fixture-bucket", "fixture-worker"
	candidate := domain.MultipartUploadInitializationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 4}
	leaseUntil := time.Now().Add(time.Minute)
	for _, test := range []struct {
		name         string
		want         error
		calls        []string
		attemptCount uint32
		retryDelay   int
	}{
		{"pre-cancelled", context.Canceled, nil, 0, 0},
		{"claim rejected", domain.ErrUploadTaskBusy, []string{"claim"}, 0, 0},
		{"stale claim result", domain.ErrInvalidUploadTaskData, []string{"claim"}, 0, 0},
		{"wrong bucket", domain.ErrInvalidStoredObjectData, []string{"claim", "find"}, 0, 0},
		{"wrong object owner", domain.ErrInvalidStoredObjectData, []string{"claim", "find"}, 0, 0},
		{"invalid session ID", domain.ErrUploadStorageUnavailable, []string{"claim", "find", "create"}, 0, 0},
		{"cancelled after create", context.Canceled, []string{"claim", "find", "create"}, 0, 0},
		{"cancelled with storage error", context.Canceled, []string{"claim", "find", "create"}, 0, 0},
		{"cancelled storage request", context.Canceled, []string{"claim", "find", "create"}, 0, 0},
		{"non-retryable storage error", domain.ErrInvalidStoredObjectData, []string{"claim", "find", "create"}, 0, 0},
		{"invalid bound result", domain.ErrInvalidUploadTaskData, []string{"claim", "find", "create", "bind"}, 0, 0},
		{"bind timeout", context.DeadlineExceeded, []string{"claim", "find", "create", "bind"}, 0, 0},
		{"first retry", nil, []string{"claim", "find", "create", "retry"}, 0, 1},
		{"storage request times out", nil, []string{"claim", "find", "create", "retry"}, 2, 4},
		{"backoff capped", nil, []string{"claim", "find", "create", "retry"}, 8, 30},
		{"retry write fails", domain.ErrUploadTaskBusy, []string{"claim", "find", "create", "retry"}, 0, 1},
		{"invalid retry result", domain.ErrInvalidUploadTaskData, []string{"claim", "find", "create", "retry"}, 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.name == "pre-cancelled" {
				cancel()
			}
			leaseOwner := owner
			task := &domain.UploadTask{
				ID: candidate.TaskID, UserID: candidate.UserID, StagedObjectID: candidate.StagedObjectID,
				UploadMode: "MULTIPART", Status: domain.UploadStatusCreating, LeaseVersion: candidate.LeaseVersion + 1,
				LeaseOwner: &leaseOwner, LeaseUntil: &leaseUntil, FileSize: 16777217, AttemptCount: test.attemptCount,
			}
			object := &domain.StoredObject{
				ID: task.StagedObjectID, OwnerUserID: task.UserID, Provider: provider, Bucket: bucket,
				ObjectKey: "users/7/objects/fixture", Status: domain.StoredObjectStatusStaging, FileSize: task.FileSize,
			}
			var calls []string
			var committedRetry *domain.UploadTask
			processor := NewMultipartUploadInitializationProcessor(provider, bucket,
				func(_ context.Context, got domain.MultipartUploadInitializationCandidate, claimedBy string) (*domain.UploadTask, error) {
					calls = append(calls, "claim")
					if got != candidate || claimedBy != owner {
						t.Fatal("领取参数发生变化")
					}
					if test.name == "claim rejected" {
						return nil, domain.ErrUploadTaskBusy
					}
					if test.name == "stale claim result" {
						task.LeaseVersion--
					}
					return task, nil
				},
				func(_ context.Context, userID, objectID uint64) (*domain.StoredObject, error) {
					calls = append(calls, "find")
					if userID != candidate.UserID || objectID != candidate.StagedObjectID {
						t.Fatal("查询参数发生变化")
					}
					if test.name == "wrong bucket" {
						object.Bucket = "other-bucket"
					}
					if test.name == "wrong object owner" {
						object.OwnerUserID++
					}
					return object, nil
				},
				func(_ context.Context, key string) (string, error) {
					calls = append(calls, "create")
					if key != object.ObjectKey {
						t.Fatal("创建未使用原对象键")
					}
					switch test.name {
					case "invalid session ID":
						return " ", nil
					case "cancelled after create":
						cancel()
					case "cancelled with storage error":
						cancel()
						return "", domain.ErrUploadStorageUnavailable
					case "cancelled storage request":
						return "", errors.Join(context.Canceled, domain.ErrUploadStorageUnavailable)
					case "non-retryable storage error":
						return "", domain.ErrInvalidStoredObjectData
					case "storage request times out":
						return "", context.DeadlineExceeded
					}
					if test.retryDelay > 0 {
						return "", domain.ErrUploadStorageUnavailable
					}
					return "fixture-session", nil
				},
				func(_ context.Context, claimed *domain.UploadTask, uploadID string) (*domain.UploadTask, error) {
					calls = append(calls, "bind")
					if claimed != task || uploadID != "fixture-session" {
						t.Fatal("绑定未使用本次租约或会话")
					}
					if test.name == "bind timeout" {
						return nil, context.DeadlineExceeded
					}
					return nil, nil
				},
				func(callContext context.Context, claimed *domain.UploadTask, delaySeconds int) (*domain.UploadTask, error) {
					calls = append(calls, "retry")
					if claimed != task || delaySeconds != test.retryDelay || callContext.Err() != nil {
						t.Fatal("重试未使用本次有效租约或预期等待时间", delaySeconds)
					}
					if test.name == "retry write fails" {
						return nil, domain.ErrUploadTaskBusy
					}
					if test.name == "invalid retry result" {
						return task, nil
					}
					committedRetry = copyUploadTask(task)
					committedRetry.LeaseOwner, committedRetry.LeaseUntil = nil, nil
					committedRetry.AttemptCount++
					committedRetry.NextAttemptAt = copyOptionalValue(&leaseUntil)
					return committedRetry, nil
				})
			result, err := processor.Process(ctx, candidate, owner)
			if result != committedRetry || !errors.Is(err, test.want) || !reflect.DeepEqual(calls, test.calls) {
				t.Fatalf("处理结果或调用顺序不符: %v %v %v", result, err, calls)
			}
		})
	}
}
