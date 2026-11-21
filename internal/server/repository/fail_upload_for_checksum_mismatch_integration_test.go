package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

type verificationFailureFixtureStream struct {
	io.Reader
	closeError error
}

func (stream verificationFailureFixtureStream) Close() error { return stream.closeError }

func TestFailUploadForChecksumMismatchIntegration(t *testing.T) {
	closeFailure := errors.New("fixture close failure")
	const multipartSize = domain.MaxSingleUploadSizeBytes + 1
	for _, scenario := range []struct {
		name           string
		multipart      bool
		zeroBytes      bool
		bareError      bool
		changeEvidence func(*domain.UploadVerificationChecksumMismatch)
		closeError     error
		changeSQL      string
		wantError      error
	}{
		{name: "mismatched digest terminates upload"},
		{name: "mismatched empty file digest", zeroBytes: true},
		{name: "bare checksum error lacks verification data", bareError: true, wantError: domain.ErrInvalidUploadTaskData},
		{name: "evidence from another task", changeEvidence: func(evidence *domain.UploadVerificationChecksumMismatch) { evidence.TaskID++ }, wantError: domain.ErrInvalidUploadTaskData},
		{name: "checksum mismatch combined with close failure", closeError: closeFailure, wantError: domain.ErrInvalidUploadTaskData},
		{name: "lease version replaced", changeSQL: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "lease elapsed", changeSQL: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "expected digest changed", changeSQL: "UPDATE upload_tasks SET expected_sha256=UNHEX(REPEAT('ab',32)) WHERE id=19", wantError: domain.ErrInvalidUploadTaskData},
		{name: "object storage version changed", changeSQL: "UPDATE stored_objects SET storage_version_id='later-version' WHERE id=23", wantError: domain.ErrInvalidStoredObjectData},
		{name: "protected staged object rolls back failure", changeSQL: "UPDATE file_entries SET object_id=23 WHERE id=31", wantError: domain.ErrStoredObjectInUse},
		{name: "multipart mismatched digest terminates upload", multipart: true},
		{name: "multipart matching digest cannot terminate", multipart: true, changeEvidence: func(evidence *domain.UploadVerificationChecksumMismatch) {
			evidence.Actual.SHA256 = evidence.ExpectedSHA256
		}, wantError: domain.ErrInvalidUploadTaskData},
		{name: "multipart incomplete observation", multipart: true, changeEvidence: func(evidence *domain.UploadVerificationChecksumMismatch) {
			evidence.Actual.SizeBytes--
		}, wantError: domain.ErrUploadBodySizeMismatch},
		{name: "multipart evidence from another lease", multipart: true, changeEvidence: func(evidence *domain.UploadVerificationChecksumMismatch) {
			evidence.LeaseVersion--
		}, wantError: domain.ErrInvalidUploadTaskData},
		{name: "multipart close also fails", multipart: true, closeError: closeFailure, wantError: domain.ErrInvalidUploadTaskData},
		{name: "multipart lease replaced", multipart: true, changeSQL: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "multipart lease elapsed", multipart: true, changeSQL: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "multipart expected digest changed", multipart: true, changeSQL: "UPDATE upload_tasks SET expected_sha256=UNHEX(REPEAT('ab',32)) WHERE id=19", wantError: domain.ErrInvalidUploadTaskData},
		{name: "multipart changed raw session", multipart: true, changeSQL: "UPDATE upload_tasks SET storage_upload_id=RTRIM(storage_upload_id) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "multipart changed layout", multipart: true, changeSQL: "UPDATE upload_tasks SET part_size=part_size-1 WHERE id=19", wantError: domain.ErrInvalidUploadTaskData},
		{name: "multipart changed version", multipart: true, changeSQL: "UPDATE stored_objects SET storage_version_id='later-version' WHERE id=23", wantError: domain.ErrInvalidStoredObjectData},
		{name: "multipart changed etag", multipart: true, changeSQL: "UPDATE stored_objects SET storage_etag='later-etag' WHERE id=23", wantError: domain.ErrInvalidStoredObjectData},
		{name: "multipart protected object rolls back failure", multipart: true, changeSQL: "UPDATE file_entries SET object_id=23 WHERE id=31", wantError: domain.ErrStoredObjectInUse},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 建立校验中的任务与对象，声明全零摘要以触发真实比较。
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
			content := []byte{0, 0xff, 0x80, 0x01}
			if scenario.zeroBytes {
				content = nil
			}
			if scenario.multipart {
				content = make([]byte, multipartSize)
			}
			size := uint64(len(content))
			const initialUsed, otherReserved uint64 = 1024, 512
			execute("UPDATE users SET used_storage=?, reserved_storage=? WHERE id=7", initialUsed, otherReserved+size)
			execute("UPDATE stored_objects SET status='VERIFYING',file_size=?,storage_version_id='verified-version' WHERE id=23", size)
			execute(`UPDATE upload_tasks SET status='VERIFYING',upload_mode='SINGLE',
				storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,
				result_entry_id=NULL,completed_at=NULL,file_size=?,reserved_bytes=?,expected_sha256=UNHEX(REPEAT('00',32)),
				lease_owner='verify-worker',lease_version=3,lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,
				expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,updated_at=UTC_TIMESTAMP(6) WHERE id=19`, size, size)
			if scenario.multipart {
				execute("UPDATE stored_objects SET storage_etag='fixture-etag' WHERE id=23")
				execute(`UPDATE upload_tasks SET upload_mode='MULTIPART',storage_upload_id='fixture-checksum-session ',
					part_size=?,part_count=2,lease_owner=NULL,lease_until=NULL,next_attempt_at=NULL WHERE id=19`, multipartSize-1)
				candidates, err := store.ListMultipartUploadVerificationCandidates(ctx, 1)
				if err != nil || len(candidates) != 1 {
					t.Fatal("未读到 Multipart 校验候选", candidates, err)
				}
				claimed, err := store.ClaimMultipartUploadVerificationLease(ctx, candidates[0], "verify-worker")
				if err != nil || claimed == nil || claimed.LeaseVersion != 4 {
					t.Fatal("未取得 Multipart 校验租约", claimed, err)
				}
			}
			readTask := func() *domain.UploadTask {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, 7, 19)
				if err != nil {
					t.Fatal(err)
				}
				return task
			}
			readStaged := func() *domain.StoredObject {
				t.Helper()
				object, err := store.FindUserStoredObject(ctx, 7, 23)
				if err != nil {
					t.Fatal(err)
				}
				return object
			}
			readLedger := func() (uint64, uint64, int) {
				t.Helper()
				var used, reserved uint64
				var fileCount int
				if err := pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,
					(SELECT COUNT(*) FROM file_entries WHERE user_id=7 AND entry_type='FILE') FROM users WHERE id=7`).Scan(&used, &reserved, &fileCount); err != nil {
					t.Fatal(err)
				}
				return used, reserved, fileCount
			}
			task, staged := readTask(), readStaged()
			_, _, originalFileCount := readLedger()

			// 2. 复用正式读回校验方法，模拟读完后证据或当前数据库状态变化。
			digest, verificationError := service.VerifyUploadObject(ctx, task, staged, func(_ context.Context, opened *domain.StoredObject) (io.ReadCloser, error) {
				if !sameStoredObjectLocation(opened, staged) || !sameUploadStorageText(opened.StorageETag, staged.StorageETag) {
					t.Fatal("校验未读取暂存对象的原位置和版本")
				}
				return verificationFailureFixtureStream{Reader: bytes.NewReader(content), closeError: scenario.closeError}, nil
			})
			if digest != nil || !errors.Is(verificationError, domain.ErrUploadChecksumMismatch) {
				t.Fatal("读回校验未获得预期摘要失配", digest, verificationError)
			}
			if scenario.closeError == nil {
				evidence, ok := verificationError.(*domain.UploadVerificationChecksumMismatch)
				if !ok || evidence.Actual.SizeBytes != size || evidence.Actual.SHA256 != sha256.Sum256(content) ||
					evidence.ExpectedSHA256 != *task.ExpectedSHA256 || evidence.Actual.SHA256 == evidence.ExpectedSHA256 {
					t.Fatal("读回未提供完整且不相符的摘要证据", verificationError)
				}
				if scenario.changeEvidence != nil {
					scenario.changeEvidence(evidence)
				}
			}
			if scenario.bareError {
				verificationError = domain.ErrUploadChecksumMismatch
			}
			if scenario.changeSQL != "" {
				execute(scenario.changeSQL)
			}
			beforeTask, beforeObject := readTask(), readStaged()
			if err := store.FailUploadForChecksumMismatch(ctx, task, staged, verificationError); !errors.Is(err, scenario.wantError) {
				t.Fatal("摘要失配收尾错误", err)
			}

			// 3. 核对任务查询、预占及对象计时，失败收尾的写入随事务一起回滚。
			afterTask, afterObject := readTask(), readStaged()
			used, reserved, fileCount := readLedger()
			if fileCount != originalFileCount {
				t.Fatal("摘要失配收尾改变了文件条目数")
			}
			if scenario.wantError != nil {
				if !reflect.DeepEqual(afterTask, beforeTask) || !reflect.DeepEqual(afterObject, beforeObject) ||
					used != initialUsed || reserved != otherReserved+size {
					t.Fatal("未通过的收尾改变了任务、对象或用量")
				}
				return
			}
			view, err := dto.NewUploadTaskData(afterTask)
			if err != nil || view.LastError == nil || view.LastError.Code != domain.UploadChecksumMismatchCode ||
				view.LastError.Message != domain.ErrUploadChecksumMismatch.Error() || view.ResultEntryID != nil ||
				afterTask.Status != domain.UploadStatusFailed || afterTask.ReservedBytes != 0 ||
				afterTask.PublishedObjectID != nil || afterTask.ResolvedName != nil ||
				afterTask.LeaseOwner != nil || afterTask.LeaseUntil != nil || afterTask.CompletedAt == nil ||
				afterTask.CompletedAt.Before(beforeTask.UpdatedAt) || used != initialUsed || reserved != otherReserved {
				t.Fatal("任务失败详情或配额不符", afterTask, err)
			}
			if afterObject.Status != domain.StoredObjectStatusOrphaned || afterObject.UnreferencedAt == nil || afterObject.GCAfter == nil ||
				!afterObject.GCAfter.Equal(afterObject.UnreferencedAt.Add(7*24*time.Hour)) || afterObject.VerifiedSHA256 != nil ||
				!sameStoredObjectLocation(afterObject, beforeObject) || !sameUploadStorageText(afterObject.StorageETag, beforeObject.StorageETag) {
				t.Fatal("摘要失配对象的回收状态不符", afterObject)
			}
			if scenario.multipart && (!sameMultipartUploadPlan(beforeTask, afterTask) || !sameUploadStorageText(afterTask.StorageUploadID, beforeTask.StorageUploadID)) {
				t.Fatal("Multipart 原分片计划或会话被改写")
			}
			if err := store.FailUploadForChecksumMismatch(ctx, task, staged, verificationError); !errors.Is(err, domain.ErrUploadTaskNotReady) {
				t.Fatal("已结束任务被重复收尾", err)
			}
			replayedUsed, replayedReserved, replayedFileCount := readLedger()
			if !reflect.DeepEqual(readTask(), afterTask) || !reflect.DeepEqual(readStaged(), afterObject) ||
				replayedUsed != used || replayedReserved != reserved || replayedFileCount != originalFileCount {
				t.Fatal("重复收尾改变了已提交结果")
			}
		})
	}
}
