package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func prepareServerUploadVerificationRecovery(t *testing.T, ctx context.Context, pool *sql.DB, target *serverUploadContentBucket) (*domain.UploadTask, string) {
	t.Helper()
	// 1. 在两次启动之间准备已持久化的待校验任务和对象。
	const objectKey, fileName = "restart-verification-object", "restart-verification.bin"
	const content, newerContent = "pending upload", "newer stored version"
	const fileSize = uint64(len(content))
	const priorLeaseVersion = 4
	uploaded, err := target.client.PutObject(ctx, target.name, objectKey, strings.NewReader(content), int64(fileSize), minio.PutObjectOptions{})
	if err != nil || uploaded.VersionID == "" || uploaded.ETag == "" {
		t.Fatal("写入重启前的暂存对象失败", err)
	}
	newer, err := target.client.PutObject(ctx, target.name, objectKey, strings.NewReader(newerContent), int64(len(newerContent)), minio.PutObjectOptions{})
	if err != nil || newer.VersionID == "" || newer.VersionID == uploaded.VersionID {
		t.Fatal("为恢复联测写入对照版本失败", err)
	}

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
			t.Fatal("准备恢复记录时没有得到有效 ID", id, err)
		}
		return uint64(id)
	}
	userID := insert(`INSERT INTO users(username,display_name,password_hash,reserved_storage)
		VALUES('verification_recovery','重启校验联测','fixture_hash',?)`, fileSize)
	deviceID := insert(`INSERT INTO user_devices(user_id,installation_id,name,platform)
		VALUES(?,'28ee758a-8727-42de-ae7d-3bb33155e148','恢复来源设备','WEB')`, userID)
	parentID := insert("INSERT INTO file_entries(user_id,name,name_key,entry_type) VALUES(?,'',X'','DIR')", userID)
	objectID := insert(`INSERT INTO stored_objects(owner_user_id,provider,bucket,object_key,storage_version_id,file_size,status,storage_etag)
		VALUES(?,'local_minio',?,?,?,?,'VERIFYING',?)`, userID, target.name, objectKey, uploaded.VersionID, fileSize, uploaded.ETag)
	expected := sha256.Sum256([]byte(content))
	taskID := insert(`INSERT INTO upload_tasks(user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,expected_sha256,file_size,
			status,reserved_bytes,lease_owner,lease_until,lease_version,expires_at)
		VALUES(?,?,?,?,?,'SINGLE',?,?,'VERIFYING',?,'previous-verifier',UTC_TIMESTAMP(6)-INTERVAL 1 SECOND,?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`,
		userID, deviceID, objectID, parentID, fileName, expected[:], fileSize, fileSize, priorLeaseVersion)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.FindUserUploadTask(ctx, userID, taskID)
	if err != nil || task.Status != domain.UploadStatusVerifying || task.LeaseVersion != priorLeaseVersion || task.ResultEntryID != nil ||
		task.ExpectedSHA256 == nil || *task.ExpectedSHA256 != expected || task.ReservedBytes != fileSize {
		t.Fatal("重启前未得到持久化的待校验状态", task, err)
	}
	return task, uploaded.VersionID
}

func verifyServerUploadVerificationRecovery(t *testing.T, ctx context.Context, pool *sql.DB, original *domain.UploadTask, storedVersion string) {
	t.Helper()
	// 2. 重新启动服务，等待后台领取并发布。
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	waitContext, stopWaiting := context.WithTimeout(ctx, 12*time.Second)
	defer stopWaiting()
	task := awaitServerUploadPublication(t, waitContext, store, original.UserID, original.ID)

	// 3. 核对原版本、发布目录项和只结算一次的配额。
	if task.LeaseVersion != original.LeaseVersion+1 || task.LeaseOwner != nil || task.LeaseUntil != nil || task.ReservedBytes != 0 ||
		task.PublishedObjectID == nil || *task.PublishedObjectID != original.StagedObjectID || task.ResultEntryID == nil {
		t.Fatal("恢复任务没有使用新租约发布原对象", task)
	}
	object, err := store.FindUserStoredObject(ctx, original.UserID, original.StagedObjectID)
	if err != nil || object.Status != domain.StoredObjectStatusReady || object.StorageVersionID == nil ||
		*object.StorageVersionID != storedVersion || object.VerifiedSHA256 == nil || *object.VerifiedSHA256 != *original.ExpectedSHA256 {
		t.Fatal("恢复校验未使用重启前保存的对象版本", object, err)
	}
	entry, err := store.FindUserFileEntry(ctx, original.UserID, *task.ResultEntryID)
	if err != nil || entry.Status != "ACTIVE" || entry.EntryType != "FILE" || entry.ParentID == nil ||
		*entry.ParentID != original.TargetParentID || entry.Name != original.RequestedName || entry.FileSize != original.FileSize {
		t.Fatal("恢复发布未生成正确的目录项", entry, err)
	}
	var used, reserved, files uint64
	if err := pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,
		(SELECT COUNT(*) FROM file_entries WHERE user_id=users.id AND entry_type='FILE') FROM users WHERE id=?`, original.UserID).Scan(&used, &reserved, &files); err != nil {
		t.Fatal(err)
	}
	if used != original.FileSize || reserved != 0 || files != 1 {
		t.Fatal("恢复发布未恰好结算一次配额", used, reserved, files)
	}
}

func verifyServerUploadVerificationRecoveryFirstRound(t *testing.T, restartLogs string) {
	t.Helper()
	// 1. 只从第二次启动日志读取 worker 开始时间和第一条非空批次。
	var startedAt time.Time
	for _, line := range strings.Split(restartLogs, "\n") {
		if !strings.Contains(line, "上传校验 worker 已启动") && !strings.Contains(line, "上传校验轮次结束") {
			continue
		}
		var event struct {
			Time            time.Time `json:"time"`
			Message         string    `json:"msg"`
			Scanned         int       `json:"scanned"`
			Published       int       `json:"published"`
			CandidateErrors int       `json:"candidate_errors"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Message == "上传校验 worker 已启动" {
			startedAt = event.Time
			continue
		}
		// 2. 恢复批次须在一次完整等待间隔过去之前完成。
		elapsed := event.Time.Sub(startedAt)
		if startedAt.IsZero() || elapsed < 0 || elapsed >= uploadVerificationInterval ||
			event.Scanned != 1 || event.Published != 1 || event.CandidateErrors != 0 {
			t.Fatal("重启首轮未完成一条恢复发布", elapsed, event)
		}
		t.Logf("恢复发布完成于 worker 启动后 %s", elapsed.Round(time.Millisecond))
		return
	}
	t.Fatal("缺少重启校验批次的完成记录")
}
