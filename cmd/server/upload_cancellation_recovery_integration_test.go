package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
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

func prepareServerUploadCancellationRecovery(t *testing.T, ctx context.Context, pool *sql.DB) *domain.UploadTask {
	t.Helper()
	// 1. 在独占库准备单独账号、有效会话和仍在上传期限内的任务。
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
		VALUES('cancellation_recovery','重启取消联测','fixture_hash',128,?,?)`, cancellationRecoveryUsedBytes, cancellationRecoveryReservedBytes)
	deviceID := insert(`INSERT INTO user_devices(user_id,installation_id,name,platform)
		VALUES(?,'06dd6b7a-f9e1-4a80-9ef2-41aae71e722d','取消恢复来源设备','WEB')`, userID)
	sessionHash := sha256.Sum256([]byte("fixture-cancellation-recovery-refresh"))
	sessionID := insert(`INSERT INTO auth_sessions(user_id,device_id,refresh_token_hash,expires_at)
		VALUES(?,?,?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`, userID, deviceID, sessionHash[:])
	parentID := insert("INSERT INTO file_entries(user_id,name,name_key,entry_type) VALUES(?,'',X'','DIR')", userID)
	objectID := insert(`INSERT INTO stored_objects(owner_user_id,provider,bucket,object_key,file_size)
		VALUES(?,'MINIO','fixture-bucket','restart-cancellation-recovery',?)`, userID, cancellationRecoveryReservedBytes)
	taskID := insert(`INSERT INTO upload_tasks(user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
		file_size,status,reserved_bytes,expires_at)
		VALUES(?,?,?,?,'restart-cancellation-recovery.bin','SINGLE',?,'UPLOADING',?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`,
		userID, deviceID, objectID, parentID, cancellationRecoveryReservedBytes, cancellationRecoveryReservedBytes)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// 2. 调用正式事务受理取消，留下已提交的中间状态供新实例接续。
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: userID, DeviceID: deviceID, SessionID: sessionID}
	pending, err := store.BeginUserUploadCancellation(ctx, actor, taskID)
	if err != nil || pending == nil || pending.Status != domain.UploadStatusCancelling || pending.ReservedBytes != cancellationRecoveryReservedBytes {
		t.Fatal("重启前未受理取消并保留任务预占", pending, err)
	}
	return pending
}

func verifyServerUploadCancellationRecovery(t *testing.T, ctx context.Context, pool *sql.DB, pending *domain.UploadTask) {
	t.Helper()
	// 1. 等待正式服务的首轮扫描接续取消收尾。
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	waitContext, stopWaiting := context.WithTimeout(ctx, 12*time.Second)
	defer stopWaiting()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var finished *domain.UploadTask
	for {
		current, err := store.FindUserUploadTask(waitContext, pending.UserID, pending.ID)
		if err != nil {
			t.Fatal("等待取消收尾时读取任务失败", err)
		}
		if current.Status == domain.UploadStatusCancelled {
			finished = current
			break
		}
		if current.Status != domain.UploadStatusCancelling {
			t.Fatal("取消处理进入意外的任务阶段", current.Status)
		}
		select {
		case <-waitContext.Done():
			t.Fatal("取消 worker 未在首轮完成收尾", waitContext.Err())
		case <-ticker.C:
		}
	}

	// 2. 核对终态、额度与数据库中的暂存对象回收时间。
	if finished.ReservedBytes != 0 || finished.CompletedAt == nil || finished.CompletedAt.Before(pending.UpdatedAt) ||
		!finished.CompletedAt.Before(pending.ExpiresAt) || finished.StagedObjectID != pending.StagedObjectID || finished.ResultEntryID != nil {
		t.Fatal("已取消任务的收尾结果不符", finished)
	}
	object, err := store.FindUserStoredObject(ctx, pending.UserID, pending.StagedObjectID)
	if err != nil || object.Status != domain.StoredObjectStatusOrphaned || object.UnreferencedAt == nil || object.GCAfter == nil ||
		!object.GCAfter.Equal(object.UnreferencedAt.Add(7*24*time.Hour)) {
		t.Fatal("取消收尾未开始暂存对象回收计时", object, err)
	}
	var used, reserved uint64
	err = pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", pending.UserID).Scan(&used, &reserved)
	if err != nil || used != cancellationRecoveryUsedBytes || reserved != 0 {
		t.Fatal("取消收尾后的用户额度不符", used, reserved, err)
	}
}

func verifyServerUploadCancellationRecoveryReport(t *testing.T, restartLogs string) {
	t.Helper()
	// 1. 服务退出后检查新实例的日志，确认只有一次非空取消轮次。
	rounds := 0
	var startedAt time.Time
	for _, line := range strings.Split(restartLogs, "\n") {
		if !strings.Contains(line, "上传取消 worker 已启动") && !strings.Contains(line, "上传取消轮次结束") {
			continue
		}
		var event struct {
			Time            time.Time `json:"time"`
			Message         string    `json:"msg"`
			Scanned         int       `json:"scanned"`
			Cancelled       int       `json:"cancelled"`
			CandidateErrors int       `json:"candidate_errors"`
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
		if startedAt.IsZero() || elapsed < 0 || elapsed >= uploadCancellationInterval ||
			event.Scanned != 1 || event.Cancelled != 1 || event.CandidateErrors != 0 {
			t.Fatal("重启首轮取消收尾结果或耗时不符", event, elapsed)
		}
		t.Logf("恢复取消完成于 worker 启动后 %s", elapsed.Round(time.Millisecond))
	}
	if rounds != 1 {
		t.Fatal("重启后应只扫描并完成一次取消任务", rounds)
	}
}
