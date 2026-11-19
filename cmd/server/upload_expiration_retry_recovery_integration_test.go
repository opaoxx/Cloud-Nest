package main

import (
	"context"
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
	expirationRetryUsedBytes     = 7
	expirationRetryReservedBytes = 9
)

type serverUploadExpirationRetryFixture struct {
	pool      *sql.DB
	store     *repository.Store
	scheduled *domain.UploadTask
}

func prepareServerUploadExpirationRetryRecovery(t *testing.T, ctx context.Context, pool *sql.DB) serverUploadExpirationRetryFixture {
	t.Helper()
	// 1. 在独占库准备单独账号和已进入 EXPIRING 的任务。
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
			t.Fatal("准备过期重试夹具时没有得到有效 ID", id, err)
		}
		return uint64(id)
	}
	userID := insert(`INSERT INTO users(username,display_name,password_hash,storage_quota,used_storage,reserved_storage)
		VALUES('expiration_retry_recovery','重启过期联测','fixture_hash',128,?,?)`, expirationRetryUsedBytes, expirationRetryReservedBytes)
	deviceID := insert(`INSERT INTO user_devices(user_id,installation_id,name,platform)
		VALUES(?,'28ee758a-9827-42de-ae7d-3bb33155e148','过期恢复来源设备','WEB')`, userID)
	parentID := insert("INSERT INTO file_entries(user_id,name,name_key,entry_type) VALUES(?,'',X'','DIR')", userID)
	const fileSize = 11
	objectID := insert(`INSERT INTO stored_objects(owner_user_id,provider,bucket,object_key,file_size)
		VALUES(?,'MINIO','fixture-bucket','restart-expiration-retry',?)`, userID, fileSize)
	// 原到期时间早于对照任务，夹具原本会优先进入到期扫描。
	taskID := insert(`INSERT INTO upload_tasks(user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
		file_size,status,reserved_bytes,lease_version,attempt_count,expires_at)
		VALUES(?,?,?,?,'restart-expiration-retry.bin','SINGLE',?,'EXPIRING',?,3,4,?)`,
		userID, deviceID, objectID, parentID, fileSize, expirationRetryReservedBytes, serverCreateUploadExpiredAt.Add(-time.Second))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// 2. 用正式写回事务安排 16 秒重试，使到期时间处在两次 30 秒扫描之间。
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.FindUserUploadTask(ctx, userID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	const retryDelaySeconds = 16
	scheduled, err := store.ScheduleUploadExpirationRetry(ctx, task, retryDelaySeconds)
	if err != nil || scheduled == nil || scheduled.NextAttemptAt == nil || scheduled.AttemptCount != task.AttemptCount+1 ||
		!scheduled.NextAttemptAt.Equal(scheduled.UpdatedAt.Add(retryDelaySeconds*time.Second)) {
		t.Fatal("启动前未得到已提交的重试安排", scheduled, err)
	}
	return serverUploadExpirationRetryFixture{pool: pool, store: store, scheduled: scheduled}
}

func (fixture serverUploadExpirationRetryFixture) verifyStillWaiting(t *testing.T, ctx context.Context) {
	t.Helper()
	// 1. 对照任务已由首轮收尾，此时等待任务应保留原快照和预占。
	scheduled := fixture.scheduled
	pending, err := fixture.store.FindUserUploadTask(ctx, scheduled.UserID, scheduled.ID)
	if err != nil || !reflect.DeepEqual(pending, scheduled) {
		t.Fatal("重启首轮改变了等待中的过期任务", pending, err)
	}
	var now time.Time
	if err := fixture.pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); err != nil || !now.Before(*scheduled.NextAttemptAt) {
		t.Fatal("未能在重试时间到达前核对首轮结果", now, err)
	}
	fixture.verifyAccount(t, ctx, expirationRetryReservedBytes)
	t.Logf("首轮处理对照任务后，数据库当前 %s，重试时间 %s，尚余 %s", now.Format(time.RFC3339Nano),
		scheduled.NextAttemptAt.Format(time.RFC3339Nano), scheduled.NextAttemptAt.Sub(now).Round(time.Millisecond))
}

func (fixture serverUploadExpirationRetryFixture) verifyRecovered(t *testing.T, ctx context.Context) {
	t.Helper()
	// 1. 保持新实例运行，读到后台继续收尾的结果为止。
	scheduled := fixture.scheduled
	waitContext, cancel := context.WithTimeout(ctx, uploadExpirationInterval+5*time.Second)
	defer cancel()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	var finished *domain.UploadTask
	for {
		task, err := fixture.store.FindUserUploadTask(waitContext, scheduled.UserID, scheduled.ID)
		if err != nil {
			t.Fatal("等待重试接续时读取任务失败", err)
		}
		if task.Status == domain.UploadStatusExpired {
			finished = task
			break
		}
		if !reflect.DeepEqual(task, scheduled) {
			t.Fatal("接续前的重试时间或任务状态发生意外变化", task)
		}
		select {
		case <-waitContext.Done():
			t.Fatal("新实例未在后续轮次收尾过期任务", waitContext.Err())
		case <-poll.C:
		}
	}

	// 2. 对照数据库时间、终态和暂存对象的回收计时。
	if finished.CompletedAt == nil || finished.CompletedAt.Before(*scheduled.NextAttemptAt) || finished.ReservedBytes != 0 ||
		finished.AttemptCount != scheduled.AttemptCount || finished.NextAttemptAt != nil || finished.LastErrorCode != nil ||
		finished.LastErrorMessage != nil || finished.ResultEntryID != nil || finished.PublishedObjectID != nil {
		t.Fatal("重试到点后的收尾结果不符", finished)
	}
	object, err := fixture.store.FindUserStoredObject(ctx, scheduled.UserID, scheduled.StagedObjectID)
	if err != nil || object.Status != domain.StoredObjectStatusOrphaned || object.UnreferencedAt == nil || object.GCAfter == nil ||
		object.UnreferencedAt.Before(*finished.CompletedAt) || !object.GCAfter.Equal(object.UnreferencedAt.Add(7*24*time.Hour)) {
		t.Fatal("过期重试接续未开始回收计时", object, err)
	}
	fixture.verifyAccount(t, ctx, 0)
	t.Logf("新实例在重试到点 %s 后完成收尾", finished.CompletedAt.Sub(*scheduled.NextAttemptAt).Round(time.Millisecond))
}

func (fixture serverUploadExpirationRetryFixture) verifyAccount(t *testing.T, ctx context.Context, wantReserved uint64) {
	t.Helper()
	// 1. 检查配额及发布副作用，确认只返还该任务的预占。
	var used, reserved, files uint64
	err := fixture.pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,
		(SELECT COUNT(*) FROM file_entries WHERE user_id=users.id AND entry_type='FILE') FROM users WHERE id=?`,
		fixture.scheduled.UserID).Scan(&used, &reserved, &files)
	if err != nil || used != expirationRetryUsedBytes || reserved != wantReserved || files != 0 {
		t.Fatal("过期重试前后的配额或文件数量不符", used, reserved, files, err)
	}
}

func verifyServerUploadExpirationRecoveryRounds(t *testing.T, restartLogs string) {
	t.Helper()
	// 1. 从新实例的日志核对两轮扫描，各自只领取当时已可处理的一条任务。
	rounds := 0
	for _, line := range strings.Split(restartLogs, "\n") {
		if !strings.Contains(line, "上传过期轮次结束") {
			continue
		}
		var report struct {
			Scanned          int `json:"scanned"`
			Expired          int `json:"expired"`
			RetriesScheduled int `json:"retries_scheduled"`
			CandidateErrors  int `json:"candidate_errors"`
		}
		rounds++
		if err := json.Unmarshal([]byte(line), &report); err != nil || report.Scanned != 1 || report.Expired != 1 ||
			report.RetriesScheduled != 0 || report.CandidateErrors != 0 {
			t.Fatal("重启后过期轮次结果不符", rounds, report, err)
		}
	}
	if rounds != 2 {
		t.Fatal("重启后应有对照任务和重试任务各一轮", rounds)
	}
}
