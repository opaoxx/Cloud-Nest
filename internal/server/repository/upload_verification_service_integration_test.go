package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUploadVerificationServiceIntegration(t *testing.T) {
	const declaredContent = "abcd"
	readFailure := errors.New("fixture read failure")
	closeFailure := errors.New("fixture close failure")
	for _, scenario := range []struct {
		name            string
		content         string
		openError       error
		readError       error
		closeError      error
		cancelRead      bool
		expiredDeadline bool
		staleLease      bool
		trashTarget     bool
		attemptCount    uint32
		wantError       error
		wantCode        string
		wantRetryDelay  time.Duration
	}{
		{name: "publish matching bytes", content: declaredContent},
		{name: "end checksum mismatch", content: "ABCD", wantError: domain.ErrUploadChecksumMismatch, wantCode: domain.UploadChecksumMismatchCode},
		{name: "end short body", content: "ab", wantError: domain.ErrUploadBodySizeMismatch, wantCode: domain.UploadBodySizeMismatchCode},
		{name: "end excess body", content: "abcde", wantError: domain.ErrUploadBodySizeMismatch, wantCode: domain.UploadBodySizeMismatchCode},
		{name: "end unavailable target", content: declaredContent, trashTarget: true, wantError: domain.ErrUploadTargetUnavailable, wantCode: domain.UploadTargetUnavailableCode},
		{name: "preserve read failure", readError: readFailure, wantError: readFailure},
		{name: "preserve checksum mismatch with close failure", content: "ABCD", closeError: closeFailure, wantError: domain.ErrUploadChecksumMismatch},
		{name: "preserve size mismatch with close failure", content: "ab", closeError: closeFailure, wantError: domain.ErrUploadBodySizeMismatch},
		{name: "preserve bare checksum error", openError: domain.ErrUploadChecksumMismatch, wantError: domain.ErrUploadChecksumMismatch},
		{name: "preserve bare size error", openError: domain.ErrUploadBodySizeMismatch, wantError: domain.ErrUploadBodySizeMismatch},
		{name: "preserve cancellation", content: declaredContent, cancelRead: true, wantError: context.Canceled},
		{name: "return stale lease instead of confirmed mismatch", content: "ab", staleLease: true, wantError: domain.ErrUploadTaskBusy},
		{name: "retry temporary open failure", openError: domain.ErrUploadStorageUnavailable, wantRetryDelay: time.Second},
		{name: "retry temporary read failure with backoff", readError: domain.ErrUploadStorageUnavailable, attemptCount: 2, wantRetryDelay: 4 * time.Second},
		{name: "retry storage timeout with active caller", openError: context.DeadlineExceeded, attemptCount: 4, wantRetryDelay: 16 * time.Second},
		{name: "retry close failure after full read", content: declaredContent, closeError: domain.ErrUploadStorageUnavailable, attemptCount: 1, wantRetryDelay: 2 * time.Second},
		{name: "retry size mismatch with temporary close failure and cap delay", content: "ab", closeError: domain.ErrUploadStorageUnavailable,
			attemptCount: 63, wantRetryDelay: 30 * time.Second},
		{name: "preserve caller cancellation with temporary open failure", cancelRead: true, openError: domain.ErrUploadStorageUnavailable, wantError: context.Canceled},
		{name: "preserve storage cancellation with active caller", openError: errors.Join(domain.ErrUploadStorageUnavailable, context.Canceled), wantError: context.Canceled},
		{name: "preserve expired caller deadline", expiredDeadline: true, wantError: context.DeadlineExceeded},
		{name: "preserve missing object", openError: domain.ErrUploadObjectMissing, wantError: domain.ErrUploadObjectMissing},
		{name: "return stale lease instead of retry result", openError: domain.ErrUploadStorageUnavailable, staleLease: true, wantError: domain.ErrUploadTaskBusy},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 在独占库准备任务、暂存对象和本次尝试持有的租约。
			pool, _ := uploadTaskDatabase(t)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			execute := func(statement string, arguments ...any) {
				t.Helper()
				if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
					t.Fatal(err)
				}
			}
			const userID, taskID, objectID, targetID = 7, 19, 23, 15
			const initialUsed, otherReserved = 1024, 512
			const size = uint64(len(declaredContent))
			expected := sha256.Sum256([]byte(declaredContent))
			execute("UPDATE users SET used_storage=?,reserved_storage=? WHERE id=?", initialUsed, otherReserved+size, userID)
			execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
				VALUES(15,7,13,'target',CAST('target' AS BINARY),'DIR')`)
			execute("UPDATE stored_objects SET status='VERIFYING',file_size=?,storage_version_id='staged-version' WHERE id=?", size, objectID)
			execute(`UPDATE upload_tasks SET status='VERIFYING',upload_mode='SINGLE',target_parent_id=?,storage_upload_id=NULL,
				part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
				file_size=?,reserved_bytes=?,expected_sha256=?,lease_owner='verify-worker',lease_version=3,
				lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,updated_at=UTC_TIMESTAMP(6),
				attempt_count=? WHERE id=?`, targetID, size, size, expected[:], scenario.attemptCount, taskID)
			readFacts := func() (*domain.UploadTask, *domain.StoredObject) {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, userID, taskID)
				if err != nil {
					t.Fatal(err)
				}
				object, err := store.FindUserStoredObject(ctx, userID, objectID)
				if err != nil {
					t.Fatal(err)
				}
				return task, object
			}
			task, staged := readFacts()
			if scenario.trashTarget {
				execute("UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=?", targetID)
			}
			if scenario.staleLease {
				execute("UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=?", taskID)
			}
			beforeTask, beforeObject := readFacts()

			// 2. 将受控对象流接给正式读回，将发布、失败及重试写回接给真实 MySQL。
			publisher := service.NewUploadPublicationService(func(ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject, digest domain.UploadContentDigest,
				publish func(context.Context, service.UploadPublicationTransaction, *domain.UploadTask, *domain.StoredObject) error) error {
				return store.WithUploadVerificationTransaction(ctx, task, staged, digest, func(txContext context.Context, txStore *Store, current *domain.UploadTask, object *domain.StoredObject) error {
					return publish(txContext, txStore, current, object)
				})
			})
			readContext, cancelRead := context.WithCancel(ctx)
			defer cancelRead()
			wantOpenCalls := 1
			if scenario.expiredDeadline {
				deadlineContext, cancelDeadline := context.WithDeadline(readContext, time.Now().Add(-time.Second))
				defer cancelDeadline()
				readContext = deadlineContext
				wantOpenCalls = 0
			}
			openCalls := 0
			verification := service.NewUploadVerificationService(func(_ context.Context, object *domain.StoredObject) (io.ReadCloser, error) {
				openCalls++
				if object != staged {
					t.Fatal("读取未使用该次校验的暂存对象")
				}
				if scenario.cancelRead {
					cancelRead()
				}
				if scenario.openError != nil {
					return nil, scenario.openError
				}
				var reader io.Reader = strings.NewReader(scenario.content)
				if scenario.readError != nil {
					reader = iotest.ErrReader(scenario.readError)
				}
				return verificationFailureFixtureStream{Reader: reader, closeError: scenario.closeError}, nil
			}, publisher.Publish, store.FailUploadForChecksumMismatch, store.FailUploadForBodySizeMismatch, store.ScheduleUploadVerificationRetry)
			result, err := verification.VerifyAndPublish(readContext, task, staged)
			if openCalls != wantOpenCalls || !errors.Is(err, scenario.wantError) {
				t.Fatal("单次校验返回错误", openCalls, result, err)
			}
			if scenario.wantRetryDelay == 0 && scenario.closeError != nil && !errors.Is(err, scenario.closeError) {
				t.Fatal("校验错误丢失关闭原因", err)
			}
			if scenario.cancelRead && scenario.openError != nil && !errors.Is(err, scenario.openError) {
				t.Fatal("取消时丢失存储错误", err)
			}
			if scenario.staleLease && (errors.Is(err, domain.ErrUploadBodySizeMismatch) || errors.Is(err, domain.ErrUploadStorageUnavailable)) {
				t.Fatal("写回失败时返回了已处理的校验原因", err)
			}

			// 3. 从事务外复核任务、对象、文件和配额的最终记录。
			savedTask, savedObject := readFacts()
			var used, reserved, files uint64
			if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", userID).Scan(&used, &reserved); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_entries WHERE user_id=? AND parent_id=?", userID, targetID).Scan(&files); err != nil {
				t.Fatal(err)
			}
			switch {
			case scenario.wantRetryDelay > 0:
				nextAttempt := savedTask.UpdatedAt.Add(scenario.wantRetryDelay)
				retryCode, retryMessage := "STORAGE_UNAVAILABLE", domain.ErrUploadStorageUnavailable.Error()
				beforeTask.LeaseOwner, beforeTask.LeaseUntil = nil, nil
				beforeTask.UpdatedAt, beforeTask.NextAttemptAt = savedTask.UpdatedAt, &nextAttempt
				beforeTask.AttemptCount++
				beforeTask.LastErrorCode, beforeTask.LastErrorMessage = &retryCode, &retryMessage
				if result == nil || !reflect.DeepEqual(result, savedTask) || !reflect.DeepEqual(savedTask, beforeTask) ||
					!reflect.DeepEqual(savedObject, beforeObject) || files != 0 || used != initialUsed || reserved != otherReserved+size {
					t.Fatal("临时存储故障未正确安排重试", result, savedTask, files, used, reserved)
				}
			case scenario.wantError == nil:
				if result == nil || !reflect.DeepEqual(result, savedTask) || savedTask.Status != domain.UploadStatusCompleted ||
					savedTask.PublishedObjectID == nil || *savedTask.PublishedObjectID != objectID || savedTask.ResultEntryID == nil ||
					savedObject.Status != domain.StoredObjectStatusReady || savedObject.VerifiedSHA256 == nil || *savedObject.VerifiedSHA256 != expected ||
					files != 1 || used != initialUsed+size || reserved != otherReserved || savedTask.ReservedBytes != 0 {
					t.Fatal("校验通过后未完整发布", result, savedTask, savedObject, files, used, reserved)
				}
			case scenario.wantCode != "":
				if result != nil || savedTask.Status != domain.UploadStatusFailed || savedTask.LastErrorCode == nil || *savedTask.LastErrorCode != scenario.wantCode ||
					savedTask.ReservedBytes != 0 || savedTask.LeaseOwner != nil || savedTask.LeaseUntil != nil ||
					savedObject.Status != domain.StoredObjectStatusOrphaned || savedObject.UnreferencedAt == nil || savedObject.GCAfter == nil ||
					files != 0 || used != initialUsed || reserved != otherReserved {
					t.Fatal("校验失败后未完整收尾", result, savedTask, savedObject, files, used, reserved)
				}
			default:
				if result != nil || !reflect.DeepEqual(savedTask, beforeTask) || !reflect.DeepEqual(savedObject, beforeObject) ||
					files != 0 || used != initialUsed || reserved != otherReserved+size {
					t.Fatal("可恢复错误留下了持久化修改", result, savedTask, savedObject, files, used, reserved)
				}
			}
		})
	}
}
