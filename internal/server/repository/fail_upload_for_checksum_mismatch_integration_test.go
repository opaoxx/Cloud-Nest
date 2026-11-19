package repository

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

type checksumMismatchFixtureStream struct {
	io.Reader
	closeError error
}

func (stream checksumMismatchFixtureStream) Close() error { return stream.closeError }

func TestFailUploadForChecksumMismatchIntegration(t *testing.T) {
	closeFailure := errors.New("fixture close failure")
	for _, scenario := range []struct {
		name          string
		zeroBytes     bool
		bareError     bool
		otherEvidence bool
		closeError    error
		changeSQL     string
		wantError     error
	}{
		{name: "mismatched digest terminates upload"},
		{name: "mismatched empty file digest", zeroBytes: true},
		{name: "bare checksum error lacks verification data", bareError: true, wantError: domain.ErrInvalidUploadTaskData},
		{name: "evidence from another task", otherEvidence: true, wantError: domain.ErrInvalidUploadTaskData},
		{name: "checksum mismatch combined with close failure", closeError: closeFailure, wantError: domain.ErrInvalidUploadTaskData},
		{name: "lease version replaced", changeSQL: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "lease elapsed", changeSQL: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "expected digest changed", changeSQL: "UPDATE upload_tasks SET expected_sha256=UNHEX(REPEAT('ab',32)) WHERE id=19", wantError: domain.ErrInvalidUploadTaskData},
		{name: "object storage version changed", changeSQL: "UPDATE stored_objects SET storage_version_id='later-version' WHERE id=23", wantError: domain.ErrInvalidStoredObjectData},
		{name: "protected staged object rolls back failure", changeSQL: "UPDATE file_entries SET object_id=23 WHERE id=31", wantError: domain.ErrStoredObjectInUse},
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
			size := uint64(len(content))
			const initialUsed, otherReserved uint64 = 1024, 512
			execute("UPDATE users SET used_storage=?, reserved_storage=? WHERE id=7", initialUsed, otherReserved+size)
			execute("UPDATE stored_objects SET status='VERIFYING',file_size=?,storage_version_id='verified-version' WHERE id=23", size)
			execute(`UPDATE upload_tasks SET status='VERIFYING',upload_mode='SINGLE',
				storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,
				result_entry_id=NULL,completed_at=NULL,file_size=?,reserved_bytes=?,expected_sha256=UNHEX(REPEAT('00',32)),
				lease_owner='verify-worker',lease_version=3,lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,
				expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,updated_at=UTC_TIMESTAMP(6) WHERE id=19`, size, size)
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
			task, staged := readTask(), readStaged()

			// 2. 复用正式读回校验方法，模拟读完后证据或当前数据库状态变化。
			digest, verificationError := service.VerifyUploadObject(ctx, task, staged, func(context.Context, *domain.StoredObject) (io.ReadCloser, error) {
				return checksumMismatchFixtureStream{Reader: bytes.NewReader(content), closeError: scenario.closeError}, nil
			})
			if digest != nil || !errors.Is(verificationError, domain.ErrUploadChecksumMismatch) {
				t.Fatal("读回校验未获得预期摘要失配", digest, verificationError)
			}
			if scenario.bareError {
				verificationError = domain.ErrUploadChecksumMismatch
			}
			if scenario.otherEvidence {
				other := *verificationError.(*domain.UploadVerificationChecksumMismatch)
				other.TaskID++
				verificationError = &other
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
			var used, reserved uint64
			if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&used, &reserved); err != nil {
				t.Fatal(err)
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
				afterTask.LeaseOwner != nil || afterTask.LeaseUntil != nil || afterTask.CompletedAt == nil ||
				afterTask.CompletedAt.Before(beforeTask.UpdatedAt) || used != initialUsed || reserved != otherReserved {
				t.Fatal("任务失败详情或配额不符", afterTask, err)
			}
			if afterObject.Status != domain.StoredObjectStatusOrphaned || afterObject.UnreferencedAt == nil || afterObject.GCAfter == nil ||
				!afterObject.GCAfter.Equal(afterObject.UnreferencedAt.Add(7*24*time.Hour)) || afterObject.VerifiedSHA256 != nil {
				t.Fatal("摘要失配对象的回收状态不符", afterObject)
			}
			if err := store.FailUploadForChecksumMismatch(ctx, task, staged, verificationError); !errors.Is(err, domain.ErrUploadTaskNotReady) {
				t.Fatal("已结束任务被重复收尾", err)
			}
		})
	}
}
