package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerReplacementConflictCopyIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_REPLACEMENT_CONFLICT_COPY_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_REPLACEMENT_CONFLICT_COPY_TEST=1 后使用独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 保留第二台设备的本地源，让两个替换任务先绑定同一个旧版本。
	fixture := prepareReplacementPublicationFixture(t, ctx)
	t.Logf("冲突副本专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	losingDevice := fixture.backupSourceFixture
	losingDevice.login = loginBackupFixture(t, ctx, &losingDevice.api, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
	if losingDevice.login.User.ID != fixture.login.User.ID || losingDevice.login.Device.ID == fixture.login.Device.ID {
		t.Fatal("冲突副本需要同一账号的两台独立设备")
	}
	winningBytes := []byte("first device accepted contents\n")
	losingBytes := []byte("second device retained local contents after conflict\n")
	sourcePath := filepath.Join(t.TempDir(), fixture.target.Name)
	if err := os.WriteFile(sourcePath, losingBytes, 0600); err != nil {
		t.Fatal("写入合成本地源失败", err)
	}
	winningDigest, localDigest := sha256.Sum256(winningBytes), sha256.Sum256(losingBytes)

	var winningTask, losingTask dto.CreateUploadData
	for _, attempt := range []struct {
		api    backupSourceClient
		key    string
		source []byte
		result *dto.CreateUploadData
	}{
		{fixture.api, "conflict-copy-winner", winningBytes, &winningTask},
		{losingDevice.api, "conflict-copy-loser", losingBytes, &losingTask},
	} {
		var created dto.CreateUploadResponse
		body := fmt.Sprintf("{\"expected_revision\":\"%d\",\"size_bytes\":%d,\"sha256\":\"%x\"}",
			fixture.target.Revision, len(attempt.source), sha256.Sum256(attempt.source))
		attempt.api.request(t, ctx, http.MethodPost, fixture.entryPath+"/content-replacement", attempt.key,
			strings.NewReader(body), http.StatusCreated, &created)
		task := created.Data.Task
		if task.Status != "UPLOADING" || task.Replacement == nil || task.Replacement.EntryID != fmt.Sprint(fixture.target.ID) ||
			task.Replacement.ExpectedRevision != fmt.Sprint(fixture.target.Revision) || created.Data.ContentPath != "/api/v1/uploads/"+task.ID+"/content" {
			t.Fatal("替换任务未绑定原文件版本或未返回对应内容路径")
		}
		*attempt.result = created.Data
	}
	if winningTask.Task.ID == losingTask.Task.ID {
		t.Fatal("两个设备共用同一替换任务")
	}

	// 2. 第一台设备发布后，第二台设备查询冲突并重新读取远端摘要。
	sendReplacementConflictCopyContent(t, ctx, fixture.api, winningTask, winningBytes)
	waitContext, stopWait := context.WithTimeout(ctx, 20*time.Second)
	defer stopWait()
	winner := awaitServerUploadPublication(t, waitContext, fixture.store, fixture.userID, backupFixtureID(t, winningTask.Task.ID))
	currentRevision := fixture.target.Revision + 1
	if winner.ReplacementPublishedRevision == nil || *winner.ReplacementPublishedRevision != currentRevision || winner.PublishedObjectID == nil {
		t.Fatal("获胜任务没有发布目标文件的新版本")
	}
	sendReplacementConflictCopyContent(t, ctx, losingDevice.api, losingTask, losingBytes)
	failure := awaitReplacementConflictCopyFailure(t, ctx, losingDevice.api, losingTask.Task.ID)
	if failure.LastError == nil || failure.LastError.Code != domain.UploadRevisionConflictCode || failure.ResultEntryID != nil ||
		failure.Replacement == nil || failure.Replacement.ConflictingRevision == nil || *failure.Replacement.ConflictingRevision != fmt.Sprint(currentRevision) {
		t.Fatal("失败设备未收到最新冲突版本和未发布结果", failure)
	}
	usedAfterReplacement := uint64(len(fixture.original) + len(winningBytes))
	fixture.checkQuota(t, ctx, usedAfterReplacement, 0)

	var remote dto.EntryDetailResponse
	losingDevice.api.request(t, ctx, http.MethodGet, fixture.entryPath, "", nil, http.StatusOK, &remote)
	var remoteDownload dto.CreateDownloadResponse
	body := fmt.Sprintf("{\"expected_revision\":%q,\"purpose\":\"download\"}", remote.Data.Revision)
	losingDevice.api.request(t, ctx, http.MethodPost, fixture.entryPath+"/download", "", strings.NewReader(body), http.StatusOK, &remoteDownload)
	if remote.Data.ID != fmt.Sprint(fixture.target.ID) || remote.Data.Revision != fmt.Sprint(currentRevision) ||
		remoteDownload.Data.SHA256 != fmt.Sprintf("%x", winningDigest) || remoteDownload.Data.SHA256 == fmt.Sprintf("%x", localDigest) {
		t.Fatal("冲突处理未读到远端新版本或两份内容未区分")
	}

	// 3. 从本地源走普通上传，发布独立副本并核对下载、配额和事件。
	retained, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Equal(retained, losingBytes) {
		t.Fatal("冲突发生后本地源没有保留", err)
	}
	extension := filepath.Ext(fixture.target.Name)
	copyName := fmt.Sprintf("%s-conflict-device-%s-%s%s", strings.TrimSuffix(fixture.target.Name, extension),
		losingDevice.login.Device.ID, uuid.NewString(), extension)
	copyID, copyObject := publishBackupSourceFile(t, ctx, losingDevice.api, fixture.store, fixture.bucket, fixture.userID,
		*fixture.target.ParentID, 1, copyName, retained)
	failedTask, err := fixture.store.FindUserUploadTask(ctx, fixture.userID, backupFixtureID(t, losingTask.Task.ID))
	if err != nil || failedTask.Status != domain.UploadStatusFailed || failedTask.DeviceID != backupFixtureID(t, losingDevice.login.Device.ID) ||
		copyID == fixture.target.ID || copyObject.ID == failedTask.StagedObjectID || copyObject.ID == *winner.PublishedObjectID {
		t.Fatal("失败任务没有保持终态，或副本没有独立的条目和对象", err)
	}
	currentObject, err := fixture.store.FindUserStoredObject(ctx, fixture.userID, *winner.PublishedObjectID)
	if err != nil {
		t.Fatal("读取获胜对象失败", err)
	}
	verifyReplacementDownload(t, ctx, losingDevice, fixture.entryPath, currentRevision, currentObject, winningBytes)
	verifyReplacementDownload(t, ctx, losingDevice, fmt.Sprintf("/api/v1/entries/%d", copyID), 1, copyObject, retained)
	stillRetained, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Equal(stillRetained, losingBytes) {
		t.Fatal("副本读回后合成本地源不符", err)
	}

	totalUsed := usedAfterReplacement + uint64(len(retained))
	fixture.checkQuota(t, ctx, totalUsed, 0)
	var sequence, histories, replacements, copyPublications uint64
	err = fixture.pool.QueryRowContext(ctx, "SELECT change_seq,"+
		"(SELECT COUNT(*) FROM file_versions WHERE user_id=users.id),"+
		"(SELECT COUNT(*) FROM change_events WHERE user_id=users.id AND event_type='FILE_CONTENT_REPLACED'),"+
		"(SELECT COUNT(*) FROM change_events WHERE user_id=users.id AND event_type='FILE_PUBLISHED' AND entry_id=? AND revision=1) "+
		"FROM users WHERE id=?", copyID, fixture.userID).Scan(&sequence, &histories, &replacements, &copyPublications)
	if err != nil || sequence != fixture.baselineSequence+2 || histories != 1 || replacements != 1 || copyPublications != 1 {
		t.Fatal("副本发布没有只新增一条普通发布事件", err)
	}

	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务退出失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	t.Logf("设备 %s，任务 %s FAILED/REVISION_CONFLICT；原文件 %d v%d：%d B SHA-256 %x；副本 %d v1：%d B SHA-256 %x",
		losingDevice.login.Device.ID, failure.ID, fixture.target.ID, currentRevision, len(winningBytes), winningDigest, copyID, len(retained), localDigest)
	t.Logf("已用 %d B，预占 0 B，历史、替换事件、副本发布事件各一条，账号游标 %d", totalUsed, sequence)
}

func sendReplacementConflictCopyContent(t *testing.T, ctx context.Context, api backupSourceClient, task dto.CreateUploadData, body []byte) {
	t.Helper()
	var accepted dto.UploadContentAcceptedResponse
	api.request(t, ctx, http.MethodPut, task.ContentPath, "", bytes.NewReader(body), http.StatusAccepted, &accepted)
	if accepted.Data.TaskID != task.Task.ID || accepted.Data.Status != "VERIFYING" {
		t.Fatal("正文接收未进入对应任务的待校验阶段", accepted.Data)
	}
}

func awaitReplacementConflictCopyFailure(t *testing.T, ctx context.Context, api backupSourceClient, taskID string) dto.UploadTaskData {
	t.Helper()
	waitContext, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()

	// 从失败设备的正式任务接口等到终态，发现其他状态则立即报告。
	for {
		var response dto.UploadTaskResponse
		api.request(t, waitContext, http.MethodGet, "/api/v1/uploads/"+taskID, "", nil, http.StatusOK, &response)
		if response.Data.ID != taskID {
			t.Fatal("任务查询返回了另一条任务", response.Data.ID)
		}
		switch response.Data.Status {
		case "FAILED":
			return response.Data
		case "VERIFYING":
		default:
			t.Fatal("替换任务进入意外状态", response.Data.Status)
		}
		select {
		case <-waitContext.Done():
			t.Fatal("未在期限内收到替换任务的冲突终态")
		case <-poll.C:
		}
	}
}
