package repository

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestFailUploadForBodySizeMismatchIntegration(t *testing.T) {
	closeFailure := errors.New("fixture close failure")
	for _, scenario := range []struct {
		name         string
		data         string
		declared     uint64
		observed     uint64
		bareError    bool
		invalidCount bool
		closeError   error
		changeSQL    string
		wantError    error
	}{
		{name: "early EOF", data: "ab", declared: 4, observed: 2},
		{name: "excess length", data: "abcdefgh", declared: 4, observed: 5},
		{name: "nonempty object for zero-byte task", data: "ab", observed: 1},
		{name: "bare size error", data: "ab", declared: 4, observed: 2, bareError: true, wantError: domain.ErrInvalidUploadTaskData},
		{name: "invalid observed amount", data: "ab", declared: 4, observed: 2, invalidCount: true, wantError: domain.ErrInvalidUploadTaskData},
		{name: "close also fails", data: "ab", declared: 4, closeError: closeFailure, wantError: domain.ErrInvalidUploadTaskData},
		{name: "new lease", data: "ab", declared: 4, observed: 2, changeSQL: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "expired lease", data: "ab", declared: 4, observed: 2, changeSQL: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "changed declaration", data: "ab", declared: 4, observed: 2, changeSQL: "UPDATE upload_tasks SET file_size=5 WHERE id=19", wantError: domain.ErrInvalidUploadTaskData},
		{name: "changed object version", data: "ab", declared: 4, observed: 2, changeSQL: "UPDATE stored_objects SET storage_version_id='different-version' WHERE id=23", wantError: domain.ErrInvalidStoredObjectData},
		{name: "protected staged object", data: "ab", declared: 4, observed: 2, changeSQL: "UPDATE file_entries SET object_id=23 WHERE id=31", wantError: domain.ErrStoredObjectInUse},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备声明与读回长度不同的任务，保留同账号的其他用量。
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
			const initialUsed, otherReserved uint64 = 1024, 512
			execute("UPDATE users SET used_storage=?,reserved_storage=? WHERE id=7", initialUsed, otherReserved+scenario.declared)
			execute("UPDATE stored_objects SET status='VERIFYING',file_size=?,storage_version_id='old-version' WHERE id=23", scenario.declared)
			execute(`UPDATE upload_tasks SET status='VERIFYING',upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,
				part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
				file_size=?,reserved_bytes=?,expected_sha256=NULL,lease_owner='verify-worker',lease_version=3,
				lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,updated_at=UTC_TIMESTAMP(6)
				WHERE id=19`, scenario.declared, scenario.declared)
			readFacts := func() (*domain.UploadTask, *domain.StoredObject, uint64, uint64) {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, 7, 19)
				if err != nil {
					t.Fatal(err)
				}
				staged, err := store.FindUserStoredObject(ctx, 7, 23)
				if err != nil {
					t.Fatal(err)
				}
				var used, reserved uint64
				if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&used, &reserved); err != nil {
					t.Fatal(err)
				}
				return task, staged, used, reserved
			}
			task, staged, _, _ := readFacts()

			// 2. 从真实验证入口取得大小错误，按场景改变证据或当前数据库记录。
			digest, verificationError := service.VerifyUploadObject(ctx, task, staged, func(context.Context, *domain.StoredObject) (io.ReadCloser, error) {
				return verificationFailureFixtureStream{Reader: strings.NewReader(scenario.data), closeError: scenario.closeError}, nil
			})
			if digest != nil || !errors.Is(verificationError, domain.ErrUploadBodySizeMismatch) {
				t.Fatal("读回未得到大小错误", digest, verificationError)
			}
			if scenario.closeError == nil {
				mismatch, ok := verificationError.(*domain.UploadVerificationSizeMismatch)
				if !ok || mismatch.DeclaredSizeBytes != scenario.declared || mismatch.ObservedSizeBytes != scenario.observed {
					t.Fatal("验证入口未提供准确的观察值", verificationError)
				}
				if scenario.invalidCount {
					mismatch.ObservedSizeBytes = scenario.declared + 2
				}
			}
			if scenario.bareError {
				verificationError = domain.ErrUploadBodySizeMismatch
			}
			if scenario.changeSQL != "" {
				execute(scenario.changeSQL)
			}
			beforeTask, beforeObject, _, _ := readFacts()
			if err := store.FailUploadForBodySizeMismatch(ctx, task, staged, verificationError); !errors.Is(err, scenario.wantError) {
				t.Fatal("大小不符收尾错误", err)
			}

			// 3. 核对事务提交或回滚，以及已结束任务的重复收尾保护。
			afterTask, afterObject, used, reserved := readFacts()
			if scenario.wantError != nil {
				if !reflect.DeepEqual(afterTask, beforeTask) || !reflect.DeepEqual(afterObject, beforeObject) ||
					used != initialUsed || reserved != otherReserved+scenario.declared {
					t.Fatal("未通过的收尾改变了任务、对象或配额")
				}
				return
			}
			view, err := dto.NewUploadTaskData(afterTask)
			if err != nil || view.LastError == nil || view.LastError.Code != domain.UploadBodySizeMismatchCode ||
				view.LastError.Message != domain.ErrUploadBodySizeMismatch.Error() || view.ResultEntryID != nil ||
				afterTask.Status != domain.UploadStatusFailed || afterTask.ReservedBytes != 0 || afterTask.FileSize != scenario.declared ||
				afterTask.LeaseOwner != nil || afterTask.LeaseUntil != nil || afterTask.CompletedAt == nil ||
				used != initialUsed || reserved != otherReserved {
				t.Fatal("任务终态或预占不符", afterTask, err)
			}
			if afterObject.Status != domain.StoredObjectStatusOrphaned || afterObject.UnreferencedAt == nil || afterObject.GCAfter == nil ||
				!afterObject.GCAfter.Equal(afterObject.UnreferencedAt.Add(7*24*time.Hour)) {
				t.Fatal("暂存对象的回收状态不符", afterObject)
			}
			if err := store.FailUploadForBodySizeMismatch(ctx, task, staged, verificationError); !errors.Is(err, domain.ErrUploadTaskNotReady) {
				t.Fatal("已结束任务被重复收尾", err)
			}
		})
	}
}
