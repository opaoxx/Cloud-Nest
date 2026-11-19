package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestProcessUploadExpirationCandidateIntegration(t *testing.T) {
	for _, scenario := range []struct {
		name             string
		multipart        bool
		changedMode      bool
		protectedObject  bool
		cancelAfterBegin bool
		wantError        error
		wantBegin        int
		wantComplete     int
	}{
		{name: "process single file to completion", wantBegin: 1, wantComplete: 1},
		{name: "skip multipart before entering expiry", multipart: true, wantError: domain.ErrUploadTaskNotReady},
		{name: "reject stale upload mode", changedMode: true, wantError: domain.ErrUploadTaskBusy, wantBegin: 1},
		{name: "resume after rollback of cleanup", protectedObject: true, wantError: domain.ErrStoredObjectInUse, wantBegin: 1, wantComplete: 1},
		{name: "resume after cancellation between transactions", cancelAfterBegin: true, wantError: context.Canceled, wantBegin: 1, wantComplete: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备可扫描的到期任务和暂存对象。
			pool, fixture := uploadTaskDatabase(t)
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
			const reserved, otherReserved, usedBytes = 5, 512, 1024
			execute("UPDATE users SET reserved_storage=?,used_storage=? WHERE id=?", reserved+otherReserved, usedBytes, fixture.UserID)
			mode := "SINGLE"
			var uploadID, partSize, partCount any
			if scenario.multipart {
				mode, uploadID, partSize, partCount = "MULTIPART", *fixture.StorageUploadID, *fixture.PartSize, *fixture.PartCount
			}
			execute(`UPDATE upload_tasks SET status='UPLOADING',upload_mode=?,storage_upload_id=?,part_size=?,part_count=?,
				published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,reserved_bytes=?,
				lease_owner=NULL,lease_until=NULL,next_attempt_at=NULL,expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 MINUTE WHERE id=?`,
				mode, uploadID, partSize, partCount, reserved, fixture.ID)
			if scenario.protectedObject {
				execute("UPDATE file_entries SET object_id=? WHERE id=?", fixture.StagedObjectID, *fixture.ResultEntryID)
			}
			scan := func() domain.UploadExpirationCandidate {
				t.Helper()
				candidates, err := store.ListUploadExpirationCandidates(ctx, 1)
				if err != nil || len(candidates) != 1 {
					t.Fatal("未取得唯一到期候选", candidates, err)
				}
				return candidates[0]
			}
			candidate := scan()
			if scenario.changedMode {
				execute("UPDATE upload_tasks SET upload_mode='MULTIPART',storage_upload_id=?,part_size=?,part_count=? WHERE id=?",
					*fixture.StorageUploadID, *fixture.PartSize, *fixture.PartCount, fixture.ID)
			}
			read := func() (*domain.UploadTask, *domain.StoredObject, uint64, uint64) {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, fixture.UserID, fixture.ID)
				if err != nil {
					t.Fatal(err)
				}
				object, err := store.FindUserStoredObject(ctx, fixture.UserID, fixture.StagedObjectID)
				if err != nil {
					t.Fatal(err)
				}
				var used, reserved uint64
				if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", fixture.UserID).
					Scan(&used, &reserved); err != nil {
					t.Fatal(err)
				}
				return task, object, used, reserved
			}
			beforeTask, beforeObject, _, _ := read()

			// 2. 通过实际扫描得到的候选调用单条处理入口。
			attemptContext, stopAttempt := context.WithCancel(ctx)
			defer stopAttempt()
			beginCalls, completeCalls := 0, 0
			processor := service.NewUploadExpirationCandidateProcessor(
				func(ctx context.Context, candidate domain.UploadExpirationCandidate) (*domain.UploadTask, error) {
					beginCalls++
					expiring, err := store.BeginUploadExpiration(ctx, candidate)
					if err == nil && scenario.cancelAfterBegin && beginCalls == 1 {
						stopAttempt()
					}
					return expiring, err
				}, func(ctx context.Context, task *domain.UploadTask) (*domain.UploadTask, error) {
					completeCalls++
					return store.CompleteUploadExpiration(ctx, task)
				}, store.ScheduleUploadExpirationRetry)
			result, err := processor.Process(attemptContext, candidate)
			if !errors.Is(err, scenario.wantError) || beginCalls != scenario.wantBegin || completeCalls != scenario.wantComplete {
				t.Fatal("候选处理结果或调用顺序错误", result, err, beginCalls, completeCalls)
			}
			if scenario.wantError != nil {
				current, object, used, reservedBytes := read()
				if result != nil || !reflect.DeepEqual(object, beforeObject) || used != usedBytes || reservedBytes != reserved+otherReserved {
					t.Fatal("失败留下了暂存对象或配额的修改", result, object, used, reservedBytes)
				}
				if scenario.wantComplete == 0 {
					if !reflect.DeepEqual(current, beforeTask) {
						t.Fatal("被拒绝的候选改变了任务", current, beforeTask)
					}
					return
				}
				if current.Status != domain.UploadStatusExpiring || current.ReservedBytes != reserved || current.CompletedAt != nil {
					t.Fatal("收尾失败后未保留已提交的 EXPIRING", current)
				}
				if scenario.protectedObject {
					execute("UPDATE file_entries SET object_id=? WHERE id=?", *fixture.PublishedObjectID, *fixture.ResultEntryID)
				}
				// 收尾中断后重扫，使用已经提交的 EXPIRING 继续。
				resumed := scan()
				if resumed.Status != domain.UploadStatusExpiring {
					t.Fatal("未扫描到可继续处理的候选", resumed)
				}
				result, err = processor.Process(ctx, resumed)
				if err != nil {
					t.Fatal("处理未能接续", err)
				}
			}

			// 3. 从事务外核对终态、回收计时及一次配额归还。
			current, object, used, reservedBytes := read()
			if result == nil || !reflect.DeepEqual(result, current) || current.Status != domain.UploadStatusExpired || current.ReservedBytes != 0 ||
				object.Status != domain.StoredObjectStatusOrphaned || object.GCAfter == nil || used != usedBytes || reservedBytes != otherReserved {
				t.Fatal("单条过期候选未正确收尾", result, current, object, used, reservedBytes)
			}
			remaining, err := store.ListUploadExpirationCandidates(ctx, 1)
			if err != nil || len(remaining) != 0 {
				t.Fatal("已终结的任务仍被扫描", remaining, err)
			}
		})
	}
}
