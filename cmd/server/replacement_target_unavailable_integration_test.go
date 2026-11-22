package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerReplacementTargetUnavailableIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_REPLACEMENT_TARGET_UNAVAILABLE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_REPLACEMENT_TARGET_UNAVAILABLE_TEST=1 后使用独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 在测试目录发布原文件，并为它创建替换任务。
	fixture := prepareBackupSource(t, ctx)
	t.Logf("替换目标不可用专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	api := fixture.api
	userID := backupFixtureID(t, fixture.login.User.ID)
	var directory dto.CreateDirectoryResponse
	api.request(t, ctx, http.MethodPost, "/api/v1/directories", "replacement-target-parent",
		strings.NewReader(fmt.Sprintf(`{"parent_id":%q,"name":"replacement-sample"}`, fixture.login.RootEntryID)), http.StatusCreated, &directory)
	parent := directory.Data.Entry
	parentID := backupFixtureID(t, parent.ID)
	original := []byte("original content under a test parent directory\n")
	entryID, originalObject := publishBackupSourceFile(t, ctx, api, store, fixture.bucket, userID, parentID, 0, "original.bin", original)
	entryPath := fmt.Sprintf("/api/v1/entries/%d", entryID)
	var originalDetail dto.EntryDetailResponse
	api.request(t, ctx, http.MethodGet, entryPath, "", nil, http.StatusOK, &originalDetail)
	originalRevision := backupFixtureID(t, originalDetail.Data.Revision)

	replacement := []byte("valid replacement content for the unavailable target\n")
	var created dto.CreateUploadResponse
	body := fmt.Sprintf(`{"expected_revision":%q,"size_bytes":%d,"sha256":"%x"}`,
		originalDetail.Data.Revision, len(replacement), sha256.Sum256(replacement))
	api.request(t, ctx, http.MethodPost, entryPath+"/content-replacement", "replacement-target-unavailable",
		strings.NewReader(body), http.StatusCreated, &created)
	taskID := backupFixtureID(t, created.Data.Task.ID)
	binding := created.Data.Task.Replacement
	if created.Data.Task.Status != "UPLOADING" || created.Data.ContentPath != "/api/v1/uploads/"+created.Data.Task.ID+"/content" ||
		binding == nil || binding.EntryID != originalDetail.Data.ID || binding.ExpectedRevision != originalDetail.Data.Revision {
		t.Fatal("替换任务未绑定回收前可见的文件版本")
	}
	checkQuota := func(wantReserved uint64) {
		t.Helper()
		var profile dto.UserProfileResponse
		api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
		if profile.Data.UsedBytes != uint64(len(original)) || profile.Data.ReservedBytes != wantReserved {
			t.Fatalf("配额不符，已用 %d B，预占 %d B，预期 %d B 和 %d B",
				profile.Data.UsedBytes, profile.Data.ReservedBytes, len(original), wantReserved)
		}
	}
	checkQuota(uint64(len(replacement)))

	// 2. 回收父目录，确认原文件不可见，再提交摘要正确的新内容。
	var trashed dto.TrashEntryResponse
	api.request(t, ctx, http.MethodPost, "/api/v1/entries/"+parent.ID+"/trash", "replacement-target-trash-parent",
		strings.NewReader(fmt.Sprintf(`{"expected_revision":%q}`, parent.Revision)), http.StatusOK, &trashed)
	if trashed.Data.Entry.ID != parent.ID || trashed.Data.Entry.Status != "TRASHED" {
		t.Fatal("测试父目录未进入回收站")
	}
	var hidden dto.ErrorResponse
	api.request(t, ctx, http.MethodGet, entryPath, "", nil, http.StatusNotFound, &hidden)
	if hidden.Error.Code != "ENTRY_NOT_FOUND" {
		t.Fatal("父目录回收后目标仍可见或错误码不符", hidden.Error.Code)
	}
	var trashSequence uint64
	if err := fixture.pool.QueryRowContext(ctx, `SELECT users.change_seq FROM users JOIN change_events event
		ON event.user_id=users.id AND event.sequence=users.change_seq
		WHERE users.id=? AND event.entry_id=? AND event.event_type='ENTRY_TRASHED'`, userID, parentID).Scan(&trashSequence); err != nil {
		t.Fatal("未找到由正式回收入账的事件", err)
	}
	sendSingleReplacementContent(t, ctx, api, created.Data, replacement)
	failed := awaitSingleReplacementFailure(t, ctx, api, created.Data.Task.ID)

	// 3. 核对发布拒绝、预占释放和事件记录，恢复目录后读回原文件。
	if failed.LastError == nil || failed.LastError.Code != domain.UploadTargetUnavailableCode || failed.ResultEntryID != nil ||
		failed.Replacement == nil || failed.Replacement.EntryID != binding.EntryID || failed.Replacement.ExpectedRevision != binding.ExpectedRevision ||
		failed.Replacement.PublishedRevision != nil || failed.Replacement.ConflictingRevision != nil {
		t.Fatal("目标不可用时任务未交付正确的失败结果", failed)
	}
	task, err := store.FindUserUploadTask(ctx, userID, taskID)
	if err != nil || task.Status != domain.UploadStatusFailed || task.PublishedObjectID != nil ||
		task.ReservedBytes != 0 || task.LeaseOwner != nil || task.LeaseUntil != nil {
		t.Fatal("失败任务仍有发布对象、预占或有效租约", err)
	}
	staged, err := store.FindUserStoredObject(ctx, userID, task.StagedObjectID)
	if err != nil || staged.Status != domain.StoredObjectStatusOrphaned || staged.StorageVersionID == nil || staged.UnreferencedAt == nil ||
		staged.GCAfter == nil || !staged.GCAfter.Equal(staged.UnreferencedAt.Add(7*24*time.Hour)) {
		t.Fatal("新对象未登记七天延迟回收", err)
	}
	checkQuota(0)
	var sequence, historyCount, replacementEvents uint64
	err = fixture.pool.QueryRowContext(ctx, `SELECT change_seq,
		(SELECT COUNT(*) FROM file_versions WHERE user_id=users.id),
		(SELECT COUNT(*) FROM change_events WHERE user_id=users.id AND event_type='FILE_CONTENT_REPLACED')
		FROM users WHERE id=?`, userID).Scan(&sequence, &historyCount, &replacementEvents)
	if err != nil || sequence != trashSequence || historyCount != 0 || replacementEvents != 0 {
		t.Fatal("替换失败改变了回收后的游标、历史或替换事件", err)
	}

	var restored dto.RestoreEntryResponse
	api.request(t, ctx, http.MethodPost, "/api/v1/trash/"+parent.ID+"/restore", "replacement-target-restore-parent",
		strings.NewReader(fmt.Sprintf(`{"expected_revision":%q}`, trashed.Data.Entry.Revision)), http.StatusOK, &restored)
	if restored.Data.Entry.ID != parent.ID || restored.Data.Entry.Status != "ACTIVE" {
		t.Fatal("测试父目录恢复结果不符", restored.Data.Entry)
	}
	verifyReplacementDownload(t, ctx, fixture, entryPath, originalRevision, originalObject, original)
	checkQuota(0)
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务退出失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
	t.Logf("父目录 %s 回收后正文进入 VERIFYING，任务 %s 最终 FAILED/TARGET_UNAVAILABLE；游标保持 %d，历史和替换事件均为 0",
		parent.ID, failed.ID, sequence)
	t.Logf("预占 %d B 全额释放，已用 %d B；恢复父目录后，原文件 %d v%d 读回 %d B，SHA-256 %x",
		len(replacement), len(original), entryID, originalRevision, len(original), sha256.Sum256(original))
}
