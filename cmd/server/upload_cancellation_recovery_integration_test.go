package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

const (
	cancellationRecoveryUsedBytes     = 7
	cancellationRecoveryReservedBytes = 9
)

type serverUploadCancellationRecoveryFixture struct {
	pool      *sql.DB
	store     *repository.Store
	ready     *domain.UploadTask
	scheduled *domain.UploadTask
}

func prepareServerUploadCancellationRecovery(t *testing.T, ctx context.Context, pool *sql.DB) serverUploadCancellationRecoveryFixture {
	t.Helper()
	// 1. 在独占库准备一个账号、一条有效会话和两条仍在上传期限内的任务。
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	insert := func(statement string, arguments ...any) uint64 {
		t.Helper()
		result, err := tx.ExecContext(ctx, statement, arguments...)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil || id <= 0 {
			t.Fatal("准备取消恢复夹具时没有得到有效 ID", id, err)
		}
		return uint64(id)
	}
	userID := insert(`INSERT INTO users(username,display_name,password_hash,storage_quota,used_storage,reserved_storage)
		VALUES('cancellation_recovery','重启取消联测','fixture_hash',128,?,?)`, cancellationRecoveryUsedBytes, 2*cancellationRecoveryReservedBytes)
	deviceID := insert(`INSERT INTO user_devices(user_id,installation_id,name,platform)
		VALUES(?,'06dd6b7a-f9e1-4a80-9ef2-41aae71e722d','取消恢复来源设备','WEB')`, userID)
	sessionHash := sha256.Sum256([]byte("fixture-cancellation-recovery-refresh"))
	sessionID := insert(`INSERT INTO auth_sessions(user_id,device_id,refresh_token_hash,expires_at)
		VALUES(?,?,?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`, userID, deviceID, sessionHash[:])
	parentID := insert("INSERT INTO file_entries(user_id,name,name_key,entry_type) VALUES(?,'',X'','DIR')", userID)
	insertTask := func(label string, attemptCount int) uint64 {
		t.Helper()
		objectKey := "restart-cancellation-" + label
		objectID := insert(`INSERT INTO stored_objects(owner_user_id,provider,bucket,object_key,file_size)
			VALUES(?,'MINIO','fixture-bucket',?,?)`, userID, objectKey, cancellationRecoveryReservedBytes)
		return insert(`INSERT INTO upload_tasks(user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
			file_size,status,reserved_bytes,attempt_count,expires_at)
			VALUES(?,?,?,?,?,'SINGLE',?,'UPLOADING',?,?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`,
			userID, deviceID, objectID, parentID, objectKey+".bin", cancellationRecoveryReservedBytes, cancellationRecoveryReservedBytes, attemptCount)
	}
	readyID := insertTask("ready", 0)
	retryID := insertTask("retry", 4)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// 2. 调用正式事务受理两条取消，给重试任务写入 16 秒等待。
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: userID, DeviceID: deviceID, SessionID: sessionID}
	accept := func(taskID uint64) *domain.UploadTask {
		t.Helper()
		pending, err := store.BeginUserUploadCancellation(ctx, actor, taskID)
		if err != nil || pending == nil || pending.Status != domain.UploadStatusCancelling || pending.ReservedBytes != cancellationRecoveryReservedBytes {
			t.Fatal("重启前未受理取消并保留任务预占", taskID, pending, err)
		}
		return pending
	}
	ready, retry := accept(readyID), accept(retryID)
	const retryDelaySeconds = 16
	scheduled, err := store.ScheduleUploadCancellationRetry(ctx, retry, retryDelaySeconds)
	if err != nil || scheduled == nil || scheduled.NextAttemptAt == nil || scheduled.AttemptCount != retry.AttemptCount+1 ||
		!scheduled.NextAttemptAt.Equal(scheduled.UpdatedAt.Add(retryDelaySeconds*time.Second)) ||
		scheduled.LastErrorCode == nil || *scheduled.LastErrorCode != "DATABASE_UNAVAILABLE" {
		t.Fatal("重启前未提交预期的取消重试安排", scheduled, err)
	}
	return serverUploadCancellationRecoveryFixture{pool: pool, store: store, ready: ready, scheduled: scheduled}
}

func (fixture serverUploadCancellationRecoveryFixture) verifyFirstRound(t *testing.T, ctx context.Context) {
	t.Helper()
	// 1. 先等对照任务完成，再核对仍在等待的任务与预占。
	fixture.waitForCancellation(t, ctx, fixture.ready, 12*time.Second)
	scheduled := fixture.scheduled
	pending, err := fixture.store.FindUserUploadTask(ctx, scheduled.UserID, scheduled.ID)
	if err != nil || !reflect.DeepEqual(pending, scheduled) {
		t.Fatal("重启首轮改变了等待中的取消任务", pending, err)
	}
	object, err := fixture.store.FindUserStoredObject(ctx, scheduled.UserID, scheduled.StagedObjectID)
	if err != nil || object.Status != domain.StoredObjectStatusStaging || object.UnreferencedAt != nil || object.GCAfter != nil {
		t.Fatal("等待任务的暂存对象提前开始了回收计时", object, err)
	}
	fixture.verifyAccount(t, ctx, cancellationRecoveryReservedBytes)
	var now time.Time
	if err := fixture.pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); err != nil || !now.Before(*scheduled.NextAttemptAt) {
		t.Fatal("未能在重试时间到达前核对取消首轮结果", now, err)
	}
	t.Logf("对照任务结束后，取消重试尚余 %s", scheduled.NextAttemptAt.Sub(now).Round(time.Millisecond))
}

func (fixture serverUploadCancellationRecoveryFixture) verifyRetryRecovered(t *testing.T, ctx context.Context) {
	t.Helper()
	// 1. 等待真实 worker 的后续轮次，核对预占已返还。
	finished := fixture.waitForCancellation(t, ctx, fixture.scheduled, uploadCancellationInterval+5*time.Second)
	fixture.verifyAccount(t, ctx, 0)
	t.Logf("新实例在取消重试到点 %s 后完成收尾", finished.CompletedAt.Sub(*fixture.scheduled.NextAttemptAt).Round(time.Millisecond))
}

func (fixture serverUploadCancellationRecoveryFixture) waitForCancellation(t *testing.T, ctx context.Context, pending *domain.UploadTask, waitLimit time.Duration) *domain.UploadTask {
	t.Helper()
	// 1. 保持服务运行，读到指定任务的取消终态。
	waitContext, stopWaiting := context.WithTimeout(ctx, waitLimit)
	defer stopWaiting()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var finished *domain.UploadTask
	for {
		current, err := fixture.store.FindUserUploadTask(waitContext, pending.UserID, pending.ID)
		if err != nil {
			t.Fatal("等待取消收尾时读取任务失败", err)
		}
		if current.Status == domain.UploadStatusCancelled {
			finished = current
			break
		}
		if !reflect.DeepEqual(current, pending) {
			t.Fatal("终结前取消任务发生意外变化", current)
		}
		select {
		case <-waitContext.Done():
			t.Fatal("取消 worker 未在期限内完成收尾", pending.ID, waitContext.Err())
		case <-ticker.C:
		}
	}

	// 2. 核对完成时间、重试诊断清理与暂存对象回收时间。
	earliestCompletion := pending.UpdatedAt
	if pending.NextAttemptAt != nil {
		earliestCompletion = *pending.NextAttemptAt
	}
	if finished.ReservedBytes != 0 || finished.CompletedAt == nil || finished.CompletedAt.Before(earliestCompletion) ||
		!finished.CompletedAt.Before(pending.ExpiresAt) || finished.StagedObjectID != pending.StagedObjectID ||
		finished.AttemptCount != pending.AttemptCount || finished.NextAttemptAt != nil || finished.LastErrorCode != nil ||
		finished.LastErrorMessage != nil || finished.ResultEntryID != nil || finished.PublishedObjectID != nil {
		t.Fatal("已取消任务的收尾结果不符", finished)
	}
	object, err := fixture.store.FindUserStoredObject(ctx, pending.UserID, pending.StagedObjectID)
	if err != nil || object.Status != domain.StoredObjectStatusOrphaned || object.UnreferencedAt == nil || object.GCAfter == nil ||
		object.UnreferencedAt.Before(*finished.CompletedAt) || !object.GCAfter.Equal(object.UnreferencedAt.Add(7*24*time.Hour)) {
		t.Fatal("取消收尾未开始暂存对象回收计时", object, err)
	}
	return finished
}

func (fixture serverUploadCancellationRecoveryFixture) verifyAccount(t *testing.T, ctx context.Context, wantReserved uint64) {
	t.Helper()
	// 1. 核对账号的预占及已用空间，并确认没有发布文件。
	var used, reserved, files uint64
	err := fixture.pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,
		(SELECT COUNT(*) FROM file_entries WHERE user_id=users.id AND entry_type='FILE') FROM users WHERE id=?`,
		fixture.ready.UserID).Scan(&used, &reserved, &files)
	if err != nil || used != cancellationRecoveryUsedBytes || reserved != wantReserved || files != 0 {
		t.Fatal("取消收尾后的账号额度或文件数量不符", used, reserved, files, err)
	}
}

func verifyServerUploadCancellationRecoveryReport(t *testing.T, restartLogs string) {
	t.Helper()
	// 1. 服务退出后检查新实例的日志，核对两次非空取消轮次。
	rounds := 0
	var startedAt time.Time
	for _, line := range strings.Split(restartLogs, "\n") {
		if !strings.Contains(line, "上传取消 worker 已启动") && !strings.Contains(line, "上传取消轮次结束") {
			continue
		}
		var event struct {
			Time             time.Time `json:"time"`
			Message          string    `json:"msg"`
			Scanned          int       `json:"scanned"`
			Cancelled        int       `json:"cancelled"`
			RetriesScheduled int       `json:"retries_scheduled"`
			CandidateErrors  int       `json:"candidate_errors"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Message == "上传取消 worker 已启动" {
			startedAt = event.Time
			continue
		}
		rounds++
		elapsed := event.Time.Sub(startedAt)
		if startedAt.IsZero() || elapsed < 0 || (rounds == 1 && elapsed >= uploadCancellationInterval) ||
			(rounds == 2 && elapsed < uploadCancellationInterval) || event.Scanned != 1 || event.Cancelled != 1 ||
			event.RetriesScheduled != 0 || event.CandidateErrors != 0 {
			t.Fatal("重启后的取消轮次结果或耗时不符", rounds, event, elapsed)
		}
		t.Logf("重启后第 %d 次非空取消轮次完成于 worker 启动后 %s", rounds, elapsed.Round(time.Millisecond))
	}
	if rounds != 2 {
		t.Fatal("重启后应分两轮完成即时任务和重试任务", rounds)
	}
}
