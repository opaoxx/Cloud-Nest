package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartUploadInitializationProcessStopsAtFailedStep(t *testing.T) {
	const provider, bucket, owner = "local_minio", "fixture-bucket", "fixture-worker"
	candidate := domain.MultipartUploadInitializationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 4}
	leaseUntil := time.Now().Add(time.Minute)
	for _, test := range []struct {
		name  string
		want  error
		calls []string
	}{
		{"pre-cancelled", context.Canceled, nil},
		{"claim rejected", domain.ErrUploadTaskBusy, []string{"claim"}},
		{"stale claim result", domain.ErrInvalidUploadTaskData, []string{"claim"}},
		{"wrong bucket", domain.ErrInvalidStoredObjectData, []string{"claim", "find"}},
		{"wrong object owner", domain.ErrInvalidStoredObjectData, []string{"claim", "find"}},
		{"invalid session ID", domain.ErrUploadStorageUnavailable, []string{"claim", "find", "create"}},
		{"cancelled after create", context.Canceled, []string{"claim", "find", "create"}},
		{"invalid bound result", domain.ErrInvalidUploadTaskData, []string{"claim", "find", "create", "bind"}},
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
				LeaseOwner: &leaseOwner, LeaseUntil: &leaseUntil, FileSize: 16777217,
			}
			object := &domain.StoredObject{
				ID: task.StagedObjectID, OwnerUserID: task.UserID, Provider: provider, Bucket: bucket,
				ObjectKey: "users/7/objects/fixture", Status: domain.StoredObjectStatusStaging, FileSize: task.FileSize,
			}
			var calls []string
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
					if test.name == "invalid session ID" {
						return " ", nil
					}
					if test.name == "cancelled after create" {
						cancel()
					}
					return "fixture-session", nil
				},
				func(_ context.Context, claimed *domain.UploadTask, uploadID string) (*domain.UploadTask, error) {
					calls = append(calls, "bind")
					if claimed != task || uploadID != "fixture-session" {
						t.Fatal("绑定未使用本次租约或会话")
					}
					return nil, nil
				})
			result, err := processor.Process(ctx, candidate, owner)
			if result != nil || !errors.Is(err, test.want) || !reflect.DeepEqual(calls, test.calls) {
				t.Fatalf("处理结果或调用顺序不符: %v %v %v", result, err, calls)
			}
		})
	}
}
