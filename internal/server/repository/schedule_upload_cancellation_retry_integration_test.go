package repository

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestScheduleUploadCancellationRetryIntegration(t *testing.T) {
	// 1. 在独占库准备已受理的取消任务，用同一连接控制 MySQL 时间。
	pool, fixture := uploadTaskDatabase(t)
	ctx := t.Context()
	pool.SetMaxOpenConns(1)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	var now time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); err != nil {
		t.Fatal(err)
	}
	now = now.Truncate(time.Second)
	setClock := func(stamp time.Time) {
		t.Helper()
		exec("SET timestamp = ?", stamp.Unix())
	}
	setClock(now)
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.ExecContext(cleanupContext, "SET timestamp = DEFAULT"); err != nil {
			t.Error("恢复测试连接时钟失败", err)
		}
	})
	exec("UPDATE users SET status='DISABLED',used_storage=1024,reserved_storage=517 WHERE id=?", fixture.UserID)
	exec(`UPDATE upload_tasks SET status='CANCELLING',upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,
		published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,reserved_bytes=5,
		expires_at=UTC_TIMESTAMP(6)+INTERVAL 10 SECOND,lease_owner=NULL,lease_until=NULL,lease_version=?,attempt_count=4,
		next_attempt_at=NULL,last_error_code=NULL,last_error_message=NULL,updated_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=?`,
		uint64(math.MaxUint64), fixture.ID)
	readTask := func() *domain.UploadTask {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, fixture.UserID, fixture.ID)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	list := func() []domain.UploadCancellationCandidate {
		t.Helper()
		candidates, err := store.ListSingleUploadCancellationCandidates(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		return candidates
	}
	checkQuota := func(wantReserved uint64) {
		t.Helper()
		var used, reserved uint64
		err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", fixture.UserID).Scan(&used, &reserved)
		if err != nil || used != 1024 || reserved != wantReserved {
			t.Fatal("账号额度与预期不符", used, reserved, err)
		}
	}
	task := readTask()
	originalCandidates := list()
	if len(originalCandidates) != 1 || originalCandidates[0].TaskID != task.ID || !now.Before(task.ExpiresAt) {
		t.Fatal("原上传期限前未找到取消候选", originalCandidates, task)
	}
	objectBefore, err := store.FindUserStoredObject(ctx, task.UserID, task.StagedObjectID)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 在原上传期限前写回重试，复核完整任务、对象和额度。
	if result, err := store.ScheduleUploadExpirationRetry(ctx, task, 30); result != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) {
		t.Fatal("过期重试入口应拒绝取消任务", result, err)
	}
	scheduled, err := store.ScheduleUploadCancellationRetry(ctx, task, 30)
	if err != nil {
		t.Fatal(err)
	}
	nextAttempt := now.Add(30 * time.Second)
	want := *task
	code, message := "DATABASE_UNAVAILABLE", "数据服务暂时不可用"
	want.NextAttemptAt, want.UpdatedAt, want.AttemptCount = &nextAttempt, now, task.AttemptCount+1
	want.LastErrorCode, want.LastErrorMessage = &code, &message
	if !reflect.DeepEqual(scheduled, &want) || !reflect.DeepEqual(readTask(), &want) {
		t.Fatal("取消重试写回的任务事实不符", scheduled, want)
	}
	objectAfter, err := store.FindUserStoredObject(ctx, task.UserID, task.StagedObjectID)
	if err != nil || !reflect.DeepEqual(objectBefore, objectAfter) {
		t.Fatal("排定重试时暂存对象发生变化", objectAfter, err)
	}
	checkQuota(517)

	// 3. 原上传期限已过时仍遵守等待；到点后拒绝旧快照，允许新快照重试和收尾。
	setClock(nextAttempt.Add(-time.Second))
	if hidden := list(); len(hidden) != 0 {
		t.Fatal("扫描提前选中了等待中的取消任务", hidden)
	}
	if result, err := store.ScheduleUploadCancellationRetry(ctx, scheduled, 1); result != nil || !errors.Is(err, domain.ErrUploadTaskBusy) {
		t.Fatal("等待中的安排被提前改写", result, err)
	}
	if result, err := store.CompleteUploadCancellation(ctx, scheduled); result != nil || !errors.Is(err, domain.ErrUploadTaskBusy) {
		t.Fatal("取消收尾绕过等待时间", result, err)
	}
	setClock(nextAttempt)
	if ready := list(); !reflect.DeepEqual(ready, originalCandidates) {
		t.Fatal("到点未恢复同一取消候选", ready)
	}
	if result, err := store.ScheduleUploadCancellationRetry(ctx, task, 30); result != nil || !errors.Is(err, domain.ErrUploadTaskBusy) {
		t.Fatal("旧快照在到点后重复推迟任务", result, err)
	}
	again, err := store.ScheduleUploadCancellationRetry(ctx, readTask(), 1)
	if err != nil || again == nil || again.AttemptCount != scheduled.AttemptCount+1 || again.NextAttemptAt == nil ||
		!again.NextAttemptAt.Equal(nextAttempt.Add(time.Second)) {
		t.Fatal("原上传期限后未能按新快照安排重试", again, err)
	}
	setClock(*again.NextAttemptAt)
	finished, err := store.CompleteUploadCancellation(ctx, again)
	if err != nil || finished == nil || !hasCompletedUploadCleanup(finished, domain.UploadStatusCancelled) || finished.AttemptCount != again.AttemptCount {
		t.Fatal("重试到点后未完成取消收尾并清理诊断", finished, err)
	}
	checkQuota(512)
}
