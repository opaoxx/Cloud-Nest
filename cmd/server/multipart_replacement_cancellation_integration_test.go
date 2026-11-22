package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerMultipartReplacementCancellationIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_REPLACEMENT_CANCELLATION_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_REPLACEMENT_CANCELLATION_TEST=1 后使用独占 MySQL、MinIO 和服务")
	}
	verifyServerMultipartReplacementCleanup(t, domain.UploadStatusCancelled)
}

func TestServerMultipartReplacementExpirationIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_REPLACEMENT_EXPIRATION_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_REPLACEMENT_EXPIRATION_TEST=1 后使用独占 MySQL、MinIO 和服务")
	}
	verifyServerMultipartReplacementCleanup(t, domain.UploadStatusExpired)
}

func verifyServerMultipartReplacementCleanup(t *testing.T, terminalStatus domain.UploadStatus) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 准备当前文件和历史，创建分片替换任务并上传第一片。
	project := "cloudnest-replacement-cleanup-" + uuid.NewString()
	t.Setenv("CLOUDNEST_MINIO_PROOF_COMPOSE_PROJECT", project)
	source := prepareBackupSourceWithMinIO(t, ctx, func(t *testing.T, ctx context.Context) string {
		return startIsolatedWebMinIOForProject(t, ctx, "128m", project)
	})
	fixture := prepareReplacementPublicationFixtureFromSource(t, ctx, source)
	t.Logf("分片替换%s专用 MySQL %s，专用 MinIO %s", terminalStatus, fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	api := fixture.api
	replacementPath := fixture.entryPath + "/content-replacement"
	createBody := func(revision uint64, content []byte) *strings.Reader {
		return strings.NewReader(fmt.Sprintf(`{"expected_revision":"%d","size_bytes":%d,"sha256":"%x"}`,
			revision, len(content), sha256.Sum256(content)))
	}
	currentBytes := []byte("current content preserved after cancelled replacement\n")
	var first dto.CreateUploadResponse
	api.request(t, ctx, http.MethodPost, replacementPath, "cancel-fixture-initial-replacement",
		createBody(fixture.target.Revision, currentBytes), http.StatusCreated, &first)
	if first.Data.ContentPath != "/api/v1/uploads/"+first.Data.Task.ID+"/content" {
		t.Fatal("准备当前版本时没有取得对应内容路径")
	}
	sendSingleReplacementContent(t, ctx, api, first.Data, currentBytes)
	waitContext, stopWait := context.WithTimeout(ctx, 20*time.Second)
	defer stopWait()
	published := awaitServerUploadPublication(t, waitContext, fixture.store, fixture.userID, backupFixtureID(t, first.Data.Task.ID))
	currentRevision := fixture.target.Revision + 1
	if published.PublishedObjectID == nil || published.ReplacementPublishedRevision == nil || *published.ReplacementPublishedRevision != currentRevision {
		t.Fatal("未能通过正式替换准备当前版本和历史")
	}
	currentObject, err := fixture.store.FindUserStoredObject(ctx, fixture.userID, *published.PublishedObjectID)
	if err != nil {
		t.Fatal("读取当前对象失败", err)
	}
	readHistory := func() domain.FileVersion {
		t.Helper()
		var history domain.FileVersion
		var count uint64
		err := fixture.pool.QueryRowContext(ctx, `SELECT id,user_id,entry_id,revision,object_id,file_size,created_at,retained_until,
			(SELECT COUNT(*) FROM file_versions WHERE user_id=?)
			FROM file_versions WHERE user_id=? AND entry_id=? AND revision=?`,
			fixture.userID, fixture.userID, fixture.target.ID, fixture.target.Revision).
			Scan(&history.ID, &history.UserID, &history.EntryID, &history.Revision, &history.ObjectID, &history.FileSize,
				&history.CreatedAt, &history.RetainedUntil, &count)
		if err != nil || count != 1 || history.ObjectID != fixture.originalObject.ID || history.FileSize != uint64(len(fixture.original)) {
			t.Fatal("应保留唯一一条原内容历史", err)
		}
		return history
	}
	historyBefore := readHistory()
	used := uint64(len(fixture.original) + len(currentBytes))
	fixture.checkQuota(t, ctx, used, 0)

	const partSize = domain.MaxSingleUploadSizeBytes
	const declaredSize = partSize + 1
	payload := make([]byte, declaredSize)
	copy(payload, "CloudNest multipart replacement to cancel")
	payload[partSize] = '2'
	var response struct {
		Data      json.RawMessage `json:"data"`
		RequestID string          `json:"request_id"`
	}
	api.request(t, ctx, http.MethodPost, replacementPath, "cancel-fixture-multipart-replacement",
		createBody(currentRevision, payload), http.StatusCreated, &response)
	created, err := dto.RestoreCreateMultipartUploadReplacementData(response.Data)
	if err != nil || response.RequestID == "" || created.PartSizeBytes != partSize || created.PartCount != 2 {
		t.Fatal("正式创建未返回两片替换计划", err)
	}
	taskID := backupFixtureID(t, created.Task.ID)
	if err := dto.ValidateCreateUploadReplacementSnapshot(response.Data, taskID, fixture.target.ID, currentRevision, declaredSize); err != nil {
		t.Fatal("分片替换任务未绑定当前版本", err)
	}
	fixture.checkQuota(t, ctx, used, declaredSize)

	taskPath := "/api/v1/uploads/" + created.Task.ID
	ready := waitForCreateUploadInitialization(t, ctx, api, taskPath)
	target := readServerMultipartPartTarget(t, ctx, fixture.backupSourceFixture, ready)
	signed, checksum := target.signPart(t, ctx, api, 1, payload[:partSize])
	etag := putMultipartProbePart(t, ctx, api.httpClient, signed, checksum, payload[:partSize], http.StatusOK, "")
	var receipts dto.UploadPartsResponse
	api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &receipts)
	expected := []dto.UploadPartReceiptData{{PartNumber: 1, SizeBytes: partSize, ETag: etag, ChecksumSHA256: checksum}}
	if receipts.Data.PartCount != 2 || !slices.Equal(receipts.Data.Parts, expected) {
		t.Fatal("应恰好收到第一片的正式回执", receipts.Data.Parts)
	}
	pending, err := fixture.store.FindUserUploadTask(ctx, fixture.userID, taskID)
	if err != nil || pending.Status != domain.UploadStatusUploading || pending.ReservedBytes != declaredSize {
		t.Fatal("收尾前应保留该任务的全部预占", err)
	}

	// 2. 按用例触发到期或取消，等待正式 worker 提交终态。
	var finished *domain.UploadTask
	if terminalStatus == domain.UploadStatusExpired {
		finished = expireServerMultipartReplacement(t, ctx, fixture, pending)
	} else {
		finished = cancelServerMultipartReplacement(t, ctx, fixture, pending, taskPath)
	}

	// 3. 核对会话、配额和历史，再通过正式接口读回当前文件。
	want, err := dto.NewUploadTaskData(finished)
	if err != nil || finished.Status != terminalStatus || want.Replacement == nil || want.Replacement.EntryID != fmt.Sprint(fixture.target.ID) ||
		want.Replacement.ExpectedRevision != fmt.Sprint(currentRevision) || want.Replacement.PublishedRevision != nil ||
		want.Replacement.ConflictingRevision != nil || finished.ReservedBytes != 0 || finished.PublishedObjectID != nil ||
		want.ResultEntryID != nil || finished.CompletedAt == nil || finished.StagedObjectID != pending.StagedObjectID ||
		finished.LeaseOwner != nil || finished.LeaseUntil != nil {
		t.Fatal("收尾终态丢失原绑定、保留预占或保存了发布结果", err)
	}
	var queried dto.UploadTaskResponse
	api.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &queried)
	if !reflect.DeepEqual(queried.Data, *want) {
		t.Fatal("查询未返回持久化的收尾终态", terminalStatus)
	}
	core := minio.Core{Client: fixture.bucket.client}
	if _, err := core.ListObjectParts(ctx, target.bucket, target.objectKey, target.uploadID, 0, 1); minio.ToErrorResponse(err).Code != "NoSuchUpload" {
		t.Fatal("正式收尾未关闭原 MinIO 分片会话", minio.ToErrorResponse(err).Code)
	}
	if _, err := fixture.bucket.client.StatObject(ctx, target.bucket, target.objectKey, minio.StatObjectOptions{}); minio.ToErrorResponse(err).Code != "NoSuchKey" {
		t.Fatal("未提交完成的分片生成了完整对象，或存储检查失败", minio.ToErrorResponse(err).Code)
	}
	fixture.checkQuota(t, ctx, used, 0)
	if historyAfter := readHistory(); historyAfter != historyBefore {
		t.Fatal("分片替换收尾改变了已有历史记录")
	}
	var sequence, eventCount uint64
	err = fixture.pool.QueryRowContext(ctx, `SELECT change_seq,(SELECT COUNT(*) FROM change_events WHERE user_id=users.id)
		FROM users WHERE id=?`, fixture.userID).Scan(&sequence, &eventCount)
	if err != nil || sequence != fixture.baselineSequence+1 || eventCount != sequence {
		t.Fatal("收尾改变了原变更位置或事件条数", err)
	}
	verifyReplacementDownload(t, ctx, fixture.backupSourceFixture, fixture.entryPath, currentRevision, currentObject, currentBytes)
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务退出失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
	t.Logf("仅上传第一片 %d B；任务 %d 查询返回 200 %s，MinIO 返回 NoSuchUpload，未生成完整对象",
		partSize, finished.ID, terminalStatus)
	t.Logf("释放 %d B，预占 0 B，已用 %d B；当前文件 %d v%d 为 %d B，SHA-256 %x；历史 v%d 和游标 %d 保持原值",
		declaredSize, used, fixture.target.ID, currentRevision, len(currentBytes), sha256.Sum256(currentBytes), historyBefore.Revision, sequence)
}

func cancelServerMultipartReplacement(t *testing.T, ctx context.Context, fixture replacementPublicationFixture, pending *domain.UploadTask, taskPath string) *domain.UploadTask {
	t.Helper()
	var accepted dto.UploadTaskResponse
	fixture.api.request(t, ctx, http.MethodPost, taskPath+"/cancel", "", nil, http.StatusAccepted, &accepted)
	binding := accepted.Data.Replacement
	if accepted.Data.ID != fmt.Sprint(pending.ID) || accepted.Data.Status != "CANCELLING" || accepted.Data.CompletedAt != nil ||
		binding == nil || binding.EntryID != fmt.Sprint(fixture.target.ID) || binding.ExpectedRevision != fmt.Sprint(fixture.target.Revision+1) {
		t.Fatal("取消受理没有返回原替换任务", accepted.Data)
	}
	finished := awaitBackupTargetCancellation(t, ctx, fixture.pool, pending)
	want, err := dto.NewUploadTaskData(finished)
	if err != nil {
		t.Fatal("读取取消终态失败", err)
	}
	var repeated dto.UploadTaskResponse
	fixture.api.request(t, ctx, http.MethodPost, taskPath+"/cancel", "", nil, http.StatusOK, &repeated)
	if !reflect.DeepEqual(repeated.Data, *want) {
		t.Fatal("重复取消未返回同一 CANCELLED 结果")
	}
	return finished
}

func expireServerMultipartReplacement(t *testing.T, ctx context.Context, fixture replacementPublicationFixture, pending *domain.UploadTask) *domain.UploadTask {
	t.Helper()
	// 1. 仅提前独占库中已上传首片的任务截止时间，由后台提交状态和配额。
	result, err := fixture.pool.ExecContext(ctx, `UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6)
		WHERE user_id=? AND id=? AND upload_mode='MULTIPART' AND status='UPLOADING'
			AND storage_upload_id IS NOT NULL AND expires_at>UTC_TIMESTAMP(6)`, fixture.userID, pending.ID)
	if err != nil {
		t.Fatal("设置合成任务截止时间失败", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		t.Fatal("仅应调整一个仍有效的分片任务", count, err)
	}

	// 2. 用后台的正式轮询间隔等待终态，拒绝取消或发布结果。
	waitContext, cancel := context.WithTimeout(ctx, uploadExpirationInterval+10*time.Second)
	defer cancel()
	poll := time.NewTicker(250 * time.Millisecond)
	defer poll.Stop()
	for {
		task, err := fixture.store.FindUserUploadTask(waitContext, fixture.userID, pending.ID)
		if err != nil {
			t.Fatal("读取待到期任务失败", err)
		}
		switch task.Status {
		case domain.UploadStatusExpired:
			if !task.ExpiresAt.Before(pending.ExpiresAt) || task.CompletedAt == nil || task.CompletedAt.Before(task.ExpiresAt) {
				t.Fatal("到期终态未使用测试截止时间")
			}
			return task
		case domain.UploadStatusUploading, domain.UploadStatusExpiring:
			if task.ReservedBytes != pending.ReservedBytes {
				t.Fatal("任务终结前已提前释放预占")
			}
		default:
			t.Fatal("分片到期任务进入意外状态", task.Status)
		}
		select {
		case <-waitContext.Done():
			t.Fatal("正式后台未在指定时间完成到期收尾")
		case <-poll.C:
		}
	}
}
