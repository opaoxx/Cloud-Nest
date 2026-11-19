package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestProcessUploadVerificationCandidateIntegration(t *testing.T) {
	lookupFailure := errors.New("fixture object query failed")
	for _, scenario := range []struct {
		name        string
		content     string
		otherClaim  bool
		lookupError error
		wrongObject bool
		openError   error
		wantStatus  domain.UploadStatus
		wantError   error
		wantLookups int
		wantOpens   int
	}{
		{name: "publish from a scanned candidate", content: "abcd", wantStatus: domain.UploadStatusCompleted, wantLookups: 1, wantOpens: 1},
		{name: "schedule retry after claiming a candidate", openError: domain.ErrUploadStorageUnavailable, wantStatus: domain.UploadStatusVerifying, wantLookups: 1, wantOpens: 1},
		{name: "finish size mismatch after claiming a candidate", content: "ab", wantStatus: domain.UploadStatusFailed,
			wantError: domain.ErrUploadBodySizeMismatch, wantLookups: 1, wantOpens: 1},
		{name: "stop when another worker claimed the candidate", otherClaim: true, wantStatus: domain.UploadStatusVerifying, wantError: domain.ErrUploadTaskBusy},
		{name: "stop on object query failure", lookupError: lookupFailure, wantStatus: domain.UploadStatusVerifying, wantError: lookupFailure, wantLookups: 1},
		{name: "stop on object from another account", wrongObject: true, wantStatus: domain.UploadStatusVerifying, wantError: domain.ErrInvalidStoredObjectData, wantLookups: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备可扫描的任务。单连接用于验证领取完成后才继续读对象。
			pool, original := uploadTaskDatabase(t)
			pool.SetMaxOpenConns(1)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			execute := func(statement string, args ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
					t.Fatal(err)
				}
			}
			const declared = "abcd"
			const fileSize = uint64(len(declared))
			const worker, otherWorker = "candidate-worker", "other-worker"
			expectedDigest := sha256.Sum256([]byte(declared))
			execute("UPDATE users SET reserved_storage=? WHERE id=?", fileSize, original.UserID)
			execute("UPDATE stored_objects SET status='VERIFYING',file_size=?,storage_version_id='claimed-version' WHERE id=?", fileSize, original.StagedObjectID)
			execute(`UPDATE upload_tasks SET status='VERIFYING',upload_mode='SINGLE',storage_upload_id=NULL,
				part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
				file_size=?,reserved_bytes=?,expected_sha256=?,lease_owner=NULL,lease_until=NULL,attempt_count=0,
				next_attempt_at=NULL,last_error_code=NULL,last_error_message=NULL,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=?`,
				fileSize, fileSize, expectedDigest[:], original.ID)
			candidates, err := store.ListUploadVerificationCandidates(ctx, 1)
			if err != nil || len(candidates) != 1 {
				t.Fatal("未扫描到等待校验的任务", candidates, err)
			}
			candidate := candidates[0]
			if scenario.otherClaim {
				if _, err := store.ClaimUploadVerificationLease(ctx, candidate, otherWorker); err != nil {
					t.Fatal(err)
				}
			}
			readTask := func() *domain.UploadTask {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, original.UserID, original.ID)
				if err != nil {
					t.Fatal(err)
				}
				return task
			}

			// 2. 接入真实领取与结果事务，并记录读取调用是否到达各自入口。
			publisher := service.NewUploadPublicationService(func(ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject, digest domain.UploadContentDigest,
				publish func(context.Context, service.UploadPublicationTransaction, *domain.UploadTask, *domain.StoredObject) error) error {
				return store.WithUploadVerificationTransaction(ctx, task, staged, digest, func(txContext context.Context, txStore *Store, current *domain.UploadTask, object *domain.StoredObject) error {
					return publish(txContext, txStore, current, object)
				})
			})
			lookups, opens := 0, 0
			verification := service.NewUploadVerificationService(func(_ context.Context, object *domain.StoredObject) (io.ReadCloser, error) {
				opens++
				if object.StorageVersionID == nil || *object.StorageVersionID != "claimed-version" {
					t.Fatal("未将查询得到的存储版本传给读回入口", object)
				}
				if scenario.openError != nil {
					return nil, scenario.openError
				}
				return io.NopCloser(strings.NewReader(scenario.content)), nil
			}, publisher.Publish, store.FailUploadForChecksumMismatch, store.FailUploadForBodySizeMismatch, store.ScheduleUploadVerificationRetry)
			processor := service.NewUploadVerificationCandidateProcessor(store.ClaimUploadVerificationLease,
				func(ctx context.Context, userID, objectID uint64) (*domain.StoredObject, error) {
					lookups++
					claimed := readTask()
					if claimed.LeaseOwner == nil || *claimed.LeaseOwner != worker || claimed.LeaseVersion != candidate.LeaseVersion+1 ||
						claimed.UserID != userID || claimed.StagedObjectID != objectID {
						t.Fatal("查询对象前未提交正确的租约", claimed, userID, objectID)
					}
					if scenario.lookupError != nil {
						return nil, scenario.lookupError
					}
					if scenario.wrongObject {
						return store.FindUserStoredObject(ctx, 8, 24)
					}
					return store.FindUserStoredObject(ctx, userID, objectID)
				}, verification.VerifyAndPublish)
			result, err := processor.Process(ctx, candidate, worker)
			if !errors.Is(err, scenario.wantError) || lookups != scenario.wantLookups || opens != scenario.wantOpens {
				t.Fatal("候选处理结果或调用顺序错误", result, err, lookups, opens)
			}

			// 3. 从事务外核对任务终态、重试记录或仍有效的租约。
			saved := readTask()
			if saved.Status != scenario.wantStatus || saved.LeaseVersion != candidate.LeaseVersion+1 ||
				(scenario.wantError == nil && !reflect.DeepEqual(result, saved)) || (scenario.wantError != nil && result != nil) {
				t.Fatal("候选结果与已提交的任务不一致", result, saved)
			}
			switch {
			case saved.Status == domain.UploadStatusCompleted:
				if saved.ResultEntryID == nil || saved.LeaseOwner != nil || saved.ReservedBytes != 0 {
					t.Fatal("候选未完成发布", saved)
				}
			case saved.Status == domain.UploadStatusFailed:
				if saved.LastErrorCode == nil || *saved.LastErrorCode != domain.UploadBodySizeMismatchCode || saved.LeaseOwner != nil || saved.ReservedBytes != 0 {
					t.Fatal("候选未完成失败收尾", saved)
				}
			case scenario.openError != nil:
				if saved.LeaseOwner != nil || saved.AttemptCount != 1 || saved.NextAttemptAt == nil ||
					!saved.NextAttemptAt.Equal(saved.UpdatedAt.Add(time.Second)) || saved.ReservedBytes != fileSize {
					t.Fatal("候选未正确安排重试", saved)
				}
			default:
				wantOwner := worker
				if scenario.otherClaim {
					wantOwner = otherWorker
				}
				if saved.LeaseOwner == nil || *saved.LeaseOwner != wantOwner || saved.LeaseUntil == nil ||
					saved.NextAttemptAt != nil || saved.ReservedBytes != fileSize {
					t.Fatal("候选错误改变了已提交租约或任务预占", saved)
				}
			}
		})
	}
}
