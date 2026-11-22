package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerMultipartRestartReconciliationIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_RESTART_RECONCILIATION_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_RESTART_RECONCILIATION_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 创建合成任务，等待后台绑定 MinIO 分片会话。
	fixture := prepareBackupSource(t, ctx)
	t.Logf("分片重启专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	const partSize = 16 << 20
	payload := make([]byte, partSize+1)
	payload[0], payload[partSize] = 0x31, 0x32
	fileSize := uint64(len(payload))
	fileDigest := sha256.Sum256(payload)
	const filename = "multipart-restart-fixture.bin"
	sourcePath := filepath.Join(t.TempDir(), filename)
	if err := os.WriteFile(sourcePath, payload, 0600); err != nil {
		t.Fatal("无法保存合成源文件", err)
	}
	declaration := fmt.Sprintf(`{"parent_id":%q,"name":%q,"size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, filename, fileSize, fileDigest)
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	fixture.api.request(t, ctx, http.MethodPost, "/api/v1/uploads", "multipart-restart-fixture",
		strings.NewReader(declaration), http.StatusCreated, &created)
	taskID := backupFixtureID(t, created.Data.Task.ID)
	if created.RequestID == "" || created.Data.Task.Status != "CREATING" ||
		created.Data.UploadMode != "MULTIPART" || created.Data.PartSizeBytes != partSize || created.Data.PartCount != 2 {
		t.Fatal("创建响应未交付固定的两片计划", created.Data)
	}
	taskPath := "/api/v1/uploads/" + created.Data.Task.ID
	readyTask := waitForCreateUploadInitialization(t, ctx, fixture.api, taskPath)
	if readyTask.ID != created.Data.Task.ID || readyTask.ParentID != fixture.login.RootEntryID || readyTask.UploadMode != "MULTIPART" ||
		readyTask.SizeBytes != fileSize || readyTask.RequestedName != filename || readyTask.ResultEntryID != nil ||
		readyTask.CompletedAt != nil || !readyTask.ExpiresAt.Equal(created.Data.ExpiresAt) {
		t.Fatal("就绪任务与原声明不符", readyTask)
	}
	target := readServerMultipartPartTarget(t, ctx, fixture, readyTask)

	// 2. 签发并上传第一片，记录重启前的任务与回执。
	uploadPart := func(api backupSourceClient, number uint32, contents []byte) dto.UploadPartReceiptData {
		t.Helper()
		uploadURL, checksum := target.signPart(t, ctx, api, number, contents)
		etag := putMultipartProbePart(t, ctx, api.httpClient, uploadURL, checksum, contents, http.StatusOK, "")
		return dto.UploadPartReceiptData{PartNumber: number, SizeBytes: uint64(len(contents)), ETag: etag, ChecksumSHA256: checksum}
	}
	firstReceipt := uploadPart(fixture.api, 1, payload[:partSize])
	readReceipts := func(api backupSourceClient, expected ...dto.UploadPartReceiptData) dto.UploadPartsData {
		t.Helper()
		var receipts dto.UploadPartsResponse
		api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &receipts)
		if receipts.RequestID == "" || receipts.Data.PartSizeBytes != partSize || receipts.Data.PartCount != 2 ||
			!slices.Equal(receipts.Data.Parts, expected) {
			t.Fatal("分片计划或实际回执不符", receipts.Data)
		}
		return receipts.Data
	}
	beforeRestart := readReceipts(fixture.api, firstReceipt)
	var beforeProfile dto.UserProfileResponse
	fixture.api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &beforeProfile)
	if beforeProfile.Data.UsedBytes != 0 || beforeProfile.Data.ReservedBytes != fileSize {
		t.Fatal("重启前的已用与预占额度不符")
	}

	// 3. 保留隔离库和桶，重启服务并重新登录。
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("第一次服务停机失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	restarted, stopRestarted := startBackupFixtureServer(t, ctx, fixture.databaseName)
	login := loginBackupFixture(t, ctx, &restarted, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
	if !reflect.DeepEqual(login.UserProfileData, beforeProfile.Data) {
		t.Fatal("重启后账号、根目录或预占额度发生变化")
	}

	// 4. 用原任务 ID 核对重启后仍可用的首片。
	var afterRestart dto.UploadTaskResponse
	restarted.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &afterRestart)
	afterRestartParts := readReceipts(restarted, firstReceipt)
	if afterRestart.RequestID == "" || !reflect.DeepEqual(afterRestart.Data, readyTask) ||
		!reflect.DeepEqual(afterRestartParts, beforeRestart) {
		t.Fatal("重启后的任务或分片回执与基线不一致")
	}
	t.Logf("重启前 %s，重启后 %s；任务 %d 保持 UPLOADING，仅第 1/2 片 %d B，ETag %s，SHA-256 %s",
		fixture.api.baseURL, restarted.baseURL, taskID, firstReceipt.SizeBytes, firstReceipt.ETag, firstReceipt.ChecksumSHA256)
	// 5. 按原计划找出缺片，核对源文件身份后仅签发并上传该片。
	var missingParts []uint32
	for number := uint32(1); number <= afterRestartParts.PartCount; number++ {
		if !slices.ContainsFunc(afterRestartParts.Parts, func(receipt dto.UploadPartReceiptData) bool {
			return receipt.PartNumber == number
		}) {
			missingParts = append(missingParts, number)
		}
	}
	if !slices.Equal(missingParts, []uint32{2}) {
		t.Fatal("缺失分片应只有第二片", missingParts)
	}
	number := missingParts[0]
	tailReceipt, err := resumeMultipartFixturePart(sourcePath, afterRestart.Data.SizeBytes, fileDigest, afterRestartParts, number,
		func(contents []byte) dto.UploadPartReceiptData {
			return uploadPart(restarted, number, contents)
		})
	if err != nil || tailReceipt.SizeBytes != 1 {
		t.Fatal("按原源文件和计划补传尾片失败", tailReceipt.SizeBytes, err)
	}
	resumedParts := readReceipts(restarted, firstReceipt, tailReceipt)
	var afterResume dto.UploadTaskResponse
	restarted.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &afterResume)
	var afterProfile dto.UserProfileResponse
	restarted.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &afterProfile)
	if !reflect.DeepEqual(afterResume.Data, readyTask) || !reflect.DeepEqual(afterProfile.Data, beforeProfile.Data) {
		t.Fatal("补传尾片后任务状态、期限或配额发生变化")
	}
	var boundSession string
	var completionRows, childEntries uint64
	if err := fixture.pool.QueryRowContext(ctx, `SELECT storage_upload_id,
		(SELECT COUNT(*) FROM upload_parts WHERE task_id=task.id),
		(SELECT COUNT(*) FROM file_entries WHERE user_id=task.user_id AND parent_id=task.target_parent_id)
		FROM upload_tasks task WHERE id=? AND user_id=?`, taskID, login.User.ID).
		Scan(&boundSession, &completionRows, &childEntries); err != nil ||
		boundSession != target.uploadID || completionRows != 0 || childEntries != 0 {
		t.Fatal("分片会话变化，或意外生成完成快照或文件条目", err, completionRows, childEntries)
	}
	t.Logf("原任务 %d 只补传第 %d 片 %d B，ETag %s，SHA-256 %s；两片齐备，首片回执保持原值，已用 %d B，预占 %d B，完成回执与发布条目均为 0",
		taskID, tailReceipt.PartNumber, tailReceipt.SizeBytes, tailReceipt.ETag, tailReceipt.ChecksumSHA256,
		afterProfile.Data.UsedBytes, afterProfile.Data.ReservedBytes)

	// 6. 使用补传后查询的编号和 ETag 提交原任务，确认 202 交付已提交的完成意图。
	accepted := submitMultipartCompletion(t, ctx, restarted, taskPath, readyTask, resumedParts.Parts...)

	// 7. 等待正式后台任务合并、校验和发布，核对文件与配额并验证下载。
	completed := awaitResumedMultipartPublication(t, ctx, restarted, taskPath, accepted)
	publication := verifyResumedMultipartPublication(t, ctx, fixture, restarted, completed, fileDigest)
	verifyResumedMultipartDownload(t, ctx, restarted, publication, fileDigest)

	// 8. 在原任务发布后无正文重试，核对完成结果和发布账目。
	verifyResumedMultipartCompletionReplay(t, ctx, fixture, restarted, taskPath, completed)

	// 9. 关停后从独立数据库连接读取冻结的回执，确认重试也保留了原完成快照。
	logs, err = stopRestarted()
	if err != nil {
		t.Fatal("重启实例停机失败", err)
	}
	verifyBackupSourceStopped(t, restarted.baseURL, logs)
	observedAt := verifyMultipartCompletionSnapshot(t, ctx, fixture, resumedParts.Parts, afterResume.Data, accepted)
	t.Logf("原任务 %d 返回 202 COMPLETING，关停后两条完成快照仍匹配，分片编号 1、2，字节数 %d、%d，共用核对时间 %s",
		taskID, firstReceipt.SizeBytes, tailReceipt.SizeBytes, observedAt.Format(time.RFC3339Nano))
}

func verifyMultipartCompletionSnapshot(t *testing.T, ctx context.Context, fixture backupSourceFixture,
	receipts []dto.UploadPartReceiptData, before, accepted dto.UploadTaskData) time.Time {
	t.Helper()
	// 1. 按原任务和账号读取经正式完成接口保存的回执。
	rows, err := fixture.pool.QueryContext(ctx, `SELECT part.part_number,part.size,part.etag,
		part.checksum_algorithm,part.checksum_value,part.observed_at
		FROM upload_parts part JOIN upload_tasks task ON task.id=part.task_id
		WHERE task.id=? AND task.user_id=? ORDER BY part.part_number`, accepted.ID, fixture.login.User.ID)
	if err != nil {
		t.Fatal("读取本次完成快照失败", err)
	}
	defer rows.Close()

	// 2. 按编号比对全部回执与同一批次的核对时间。
	var firstObservedAt time.Time
	for _, expected := range receipts {
		if !rows.Next() {
			t.Fatal("完成快照缺少预期分片", expected.PartNumber, rows.Err())
		}
		var persisted dto.UploadPartReceiptData
		var algorithm string
		var observedAt time.Time
		if err := rows.Scan(&persisted.PartNumber, &persisted.SizeBytes, &persisted.ETag,
			&algorithm, &persisted.ChecksumSHA256, &observedAt); err != nil {
			t.Fatal("读取完成回执字段失败", err)
		}
		if firstObservedAt.IsZero() {
			firstObservedAt = observedAt
		}
		if persisted != expected || algorithm != "SHA256" || !observedAt.Equal(firstObservedAt) ||
			observedAt.Before(before.UpdatedAt) || observedAt.After(accepted.UpdatedAt) {
			t.Fatal("已保存回执与正式查询或本轮核对时间不一致", persisted, algorithm, observedAt)
		}
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatal("完成快照包含额外分片或读取失败", rows.Err())
	}
	return firstObservedAt
}

type serverMultipartPartTarget struct {
	taskPath       string
	bucket         string
	objectKey      string
	uploadID       string
	taskExpiration time.Time
}

func readServerMultipartPartTarget(t *testing.T, ctx context.Context, fixture backupSourceFixture, task dto.UploadTaskData) serverMultipartPartTarget {
	t.Helper()
	// 1. 按任务和所有权读取本轮专用桶已绑定的原始会话。
	target := serverMultipartPartTarget{
		taskPath: "/api/v1/uploads/" + task.ID, bucket: fixture.bucket.name, taskExpiration: task.ExpiresAt,
	}
	if err := fixture.pool.QueryRowContext(ctx, `SELECT task.storage_upload_id, obj.object_key
		FROM upload_tasks task JOIN stored_objects obj ON obj.id=task.staged_object_id
		WHERE task.id=? AND task.user_id=? AND obj.bucket=?`, task.ID, fixture.login.User.ID, target.bucket).
		Scan(&target.uploadID, &target.objectKey); err != nil || target.uploadID == "" || target.objectKey == "" {
		t.Fatal("专用桶分片会话尚未绑定", err)
	}

	// 2. 测试结束后只清理这个专用会话，已完成的会话会由存储移除。
	core := minio.Core{Client: fixture.bucket.client}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := core.AbortMultipartUpload(cleanup, target.bucket, target.objectKey, target.uploadID); err != nil &&
			minio.ToErrorResponse(err).Code != "NoSuchUpload" {
			t.Error("合成分片会话清理失败", minio.ToErrorResponse(err).Code)
		}
	})
	return target
}

func (target serverMultipartPartTarget) signPart(t *testing.T, ctx context.Context, api backupSourceClient, number uint32, contents []byte) (*url.URL, string) {
	t.Helper()
	// 1. 通过正式接口请求绑定完整正文摘要的单片签名。
	checksum := multipartProbeSHA256(contents)
	var signed dto.UploadPartURLsResponse
	api.request(t, ctx, http.MethodPost, target.taskPath+"/part-urls", "",
		strings.NewReader(fmt.Sprintf(`{"parts":[{"part_number":%d,"sha256":"%x"}]}`, number, sha256.Sum256(contents))),
		http.StatusOK, &signed)
	if signed.RequestID == "" || len(signed.Data.Parts) != 1 || !signed.Data.ExpiresAt.Equal(target.taskExpiration) {
		t.Fatal("分片签发范围或任务期限不符", number)
	}

	// 2. 校验链接只能写入本轮回环存储中指定会话的预期分片。
	part := signed.Data.Parts[0]
	uploadURL, err := url.Parse(part.URL)
	if err != nil || uploadURL.Scheme+"://"+uploadURL.Host != os.Getenv("CLOUDNEST_S3_PUBLIC_ENDPOINT") || uploadURL.User != nil ||
		uploadURL.Path != "/"+target.bucket+"/"+target.objectKey || uploadURL.Query().Get("uploadId") != target.uploadID ||
		uploadURL.Query().Get("partNumber") != fmt.Sprint(number) || part.PartNumber != number ||
		part.SizeBytes != uint64(len(contents)) || part.RequiredHeaders.ChecksumSHA256 != checksum {
		t.Fatal("签名没有限定在本轮隔离会话的预期分片", number)
	}
	return uploadURL, checksum
}

func submitMultipartCompletion(t *testing.T, ctx context.Context, api backupSourceClient, taskPath string,
	task dto.UploadTaskData, receipts ...dto.UploadPartReceiptData) dto.UploadTaskData {
	t.Helper()
	// 1. 只提交正式查询返回的分片编号和 ETag。
	type partClaim struct {
		PartNumber uint32 `json:"part_number"`
		ETag       string `json:"etag"`
	}
	claims := make([]partClaim, 0, len(receipts))
	for _, receipt := range receipts {
		claims = append(claims, partClaim{PartNumber: receipt.PartNumber, ETag: receipt.ETag})
	}
	completeBody, err := json.Marshal(struct {
		Parts []partClaim `json:"parts"`
	}{Parts: claims})
	if err != nil {
		t.Fatal("构造完成请求失败", err)
	}
	var accepted dto.UploadTaskResponse
	api.request(t, ctx, http.MethodPost, taskPath+"/complete", "", bytes.NewReader(completeBody), http.StatusAccepted, &accepted)

	// 2. 受理结果使用原任务声明，此时尚没有发布条目。
	if accepted.RequestID == "" || accepted.Data.ID != task.ID || accepted.Data.UploadMode != "MULTIPART" ||
		accepted.Data.Status != "COMPLETING" || accepted.Data.ParentID != task.ParentID || accepted.Data.RequestedName != task.RequestedName ||
		accepted.Data.SizeBytes != task.SizeBytes || !accepted.Data.ExpiresAt.Equal(task.ExpiresAt) ||
		accepted.Data.ResultEntryID != nil || accepted.Data.CompletedAt != nil {
		t.Fatal("完成请求未交付原任务的 COMPLETING 快照", accepted.Data)
	}
	return accepted.Data
}

func awaitResumedMultipartPublication(t *testing.T, ctx context.Context, api backupSourceClient, taskPath string,
	accepted dto.UploadTaskData) dto.UploadTaskData {
	t.Helper()
	waitContext, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	// 1. 通过正式查询等待终态，合并和校验可能在两次查询之间完成。
	for {
		var result dto.UploadTaskResponse
		api.request(t, waitContext, http.MethodGet, taskPath, "", nil, http.StatusOK, &result)
		switch result.Data.Status {
		case "COMPLETED":
			completed := result.Data
			if result.RequestID == "" || completed.ID != accepted.ID || completed.UploadMode != "MULTIPART" ||
				completed.ParentID != accepted.ParentID || completed.ResultEntryID == nil || completed.CompletedAt == nil ||
				completed.ResolvedName == nil || *completed.ResolvedName != accepted.RequestedName ||
				completed.SizeBytes != accepted.SizeBytes || completed.LastError != nil || completed.CompletedAt.Before(accepted.UpdatedAt) {
				t.Fatal("后台发布结果与原续传任务不一致", completed)
			}
			return completed
		case "COMPLETING", "VERIFYING":
		default:
			t.Fatal("续传发布进入意外状态", result.Data.Status, result.Data.LastError)
		}
		select {
		case <-waitContext.Done():
			t.Fatal("续传任务未在期限内完成发布", result.Data.Status, result.Data.LastError)
		case <-poll.C:
		}
	}
}

type resumedMultipartPublication struct {
	entry                     dto.EntryDetailData
	objectID                  uint64
	objectPath, objectVersion string
}

func verifyResumedMultipartPublication(t *testing.T, ctx context.Context, fixture backupSourceFixture, api backupSourceClient,
	completed dto.UploadTaskData, fileDigest [sha256.Size]byte) resumedMultipartPublication {
	t.Helper()
	// 1. 用任务返回的 ID 读取活动文件，核对父目录、名称及大小。
	entryID := *completed.ResultEntryID
	var entry dto.EntryDetailResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/entries/"+entryID, "", nil, http.StatusOK, &entry)
	if entry.RequestID == "" || entry.Data.ID != entryID || entry.Data.ParentID == nil ||
		*entry.Data.ParentID != fixture.login.RootEntryID || entry.Data.Name != *completed.ResolvedName ||
		entry.Data.Status != "ACTIVE" || entry.Data.Kind == "folder" || entry.Data.SizeBytes != completed.SizeBytes {
		t.Fatal("发布条目与续传任务的结果不一致", entry.Data)
	}

	// 2. 核对任务和条目引用同一个已校验对象，并确认只发布了一份文件。
	var objectID, objectSize, entryCount uint64
	var objectState, objectKey, objectVersion string
	var verifiedDigest []byte
	if err := fixture.pool.QueryRowContext(ctx, `SELECT obj.id,obj.file_size,obj.status,obj.object_key,obj.storage_version_id,obj.verified_sha256,
		(SELECT COUNT(*) FROM file_entries WHERE user_id=task.user_id AND parent_id=task.target_parent_id AND entry_type='FILE' AND status='ACTIVE')
		FROM upload_tasks task
		JOIN file_entries entry ON entry.user_id=task.user_id AND entry.id=task.result_entry_id AND entry.object_id=task.published_object_id
		JOIN stored_objects obj ON obj.owner_user_id=entry.user_id AND obj.id=entry.object_id
		WHERE task.id=? AND task.user_id=? AND entry.id=? AND task.status='COMPLETED' AND task.reserved_bytes=0 AND obj.bucket=?`,
		completed.ID, fixture.login.User.ID, entryID, fixture.bucket.name).
		Scan(&objectID, &objectSize, &objectState, &objectKey, &objectVersion, &verifiedDigest, &entryCount); err != nil ||
		objectID == 0 || objectSize != completed.SizeBytes || objectState != "READY" || objectKey == "" || objectVersion == "" ||
		!bytes.Equal(verifiedDigest, fileDigest[:]) || entryCount != 1 {
		t.Fatal("发布对象的定位、摘要或文件条目数量不符", err, objectState, entryCount)
	}

	// 3. 用公开资料确认这一个文件的大小已从预占转为已用。
	var profile dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	if profile.Data.User.ID != fixture.login.User.ID || profile.Data.UsedBytes != completed.SizeBytes || profile.Data.ReservedBytes != 0 {
		t.Fatal("续传发布后的账号配额不符", profile.Data.UsedBytes, profile.Data.ReservedBytes)
	}
	t.Logf("原任务 %s 达到 COMPLETED，发布文件条目 %s，引用 READY 对象 %d，版本已定位，整文件 SHA-256 %x，已用 %d B、预占 %d B",
		completed.ID, entryID, objectID, verifiedDigest, profile.Data.UsedBytes, profile.Data.ReservedBytes)
	return resumedMultipartPublication{
		entry: entry.Data, objectID: objectID, objectPath: "/" + fixture.bucket.name + "/" + objectKey, objectVersion: objectVersion,
	}
}

func verifyResumedMultipartDownload(t *testing.T, ctx context.Context, api backupSourceClient, publication resumedMultipartPublication,
	fileDigest [sha256.Size]byte) {
	t.Helper()
	entry := publication.entry
	// 1. 使用可见条目的当前版本申请下载，核对授权给出的对象和内容事实。
	body := fmt.Sprintf(`{"expected_revision":%q,"purpose":"download"}`, entry.Revision)
	var grant dto.CreateDownloadResponse
	api.request(t, ctx, http.MethodPost, "/api/v1/entries/"+entry.ID+"/download", "", strings.NewReader(body), http.StatusOK, &grant)
	if grant.RequestID == "" || grant.Data.Revision != entry.Revision || grant.Data.ObjectID != strconv.FormatUint(publication.objectID, 10) ||
		grant.Data.SizeBytes != entry.SizeBytes || grant.Data.SHA256 != fmt.Sprintf("%x", fileDigest) || !grant.Data.ExpiresAt.After(time.Now()) {
		t.Fatal("下载授权的条目版本、对象或摘要不符", entry.ID)
	}

	// 2. 限定签名链接指向本轮隔离桶及已发布对象的确切版本。
	signed, err := url.Parse(grant.Data.URL)
	if err != nil || signed.User != nil || signed.Fragment != "" ||
		signed.Scheme+"://"+signed.Host != os.Getenv("CLOUDNEST_S3_PUBLIC_ENDPOINT") ||
		signed.Path != publication.objectPath || signed.Query().Get("versionId") != publication.objectVersion {
		t.Fatal("下载签名没有绑定本轮隔离对象及已发布版本", entry.ID)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, signed.String(), nil)
	if err != nil {
		t.Fatal("无法构造隔离签名下载请求", entry.ID)
	}
	request.Header.Set("Accept-Encoding", "identity")
	response, err := api.httpClient.Do(request)
	if err != nil {
		t.Fatal("访问隔离签名下载地址失败", entry.ID)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength != int64(entry.SizeBytes) ||
		response.Header.Get("X-Amz-Version-Id") != publication.objectVersion {
		t.Fatalf("隔离存储返回的版本或长度不符，HTTP %d，长度 %d", response.StatusCode, response.ContentLength)
	}

	// 3. 用固定缓冲区读取实际正文，多留一字节的读取额度用于检查超长内容。
	digest := sha256.New()
	buffer := make([]byte, 64<<10)
	size, err := io.CopyBuffer(digest, io.LimitReader(response.Body, int64(entry.SizeBytes)+1), buffer)
	downloadDigest := digest.Sum(nil)
	if err != nil || size != int64(entry.SizeBytes) || !bytes.Equal(downloadDigest, fileDigest[:]) {
		t.Fatal("下载正文与合成源不一致", entry.ID, size, err)
	}
	t.Logf("正式下载条目 %s，存储返回 HTTP 200 和已发布版本，用 %d B 缓冲区读取 %d B，下载 SHA-256 %x，与合成源一致",
		entry.ID, len(buffer), size, downloadDigest)
}

func verifyResumedMultipartCompletionReplay(t *testing.T, ctx context.Context, fixture backupSourceFixture, api backupSourceClient,
	taskPath string, completed dto.UploadTaskData) {
	t.Helper()
	// 1. 从同一次只读查询记录发布对象、账号配额及文件、对象和回执数量。
	type completionState struct {
		publishedObjectID, usedBytes, reservedBytes uint64
		files, objects, receipts                    uint64
	}
	readState := func() completionState {
		t.Helper()
		var state completionState
		if err := fixture.pool.QueryRowContext(ctx, `SELECT task.published_object_id,account.used_storage,account.reserved_storage,
			(SELECT COUNT(*) FROM file_entries WHERE user_id=account.id AND entry_type='FILE'),
			(SELECT COUNT(*) FROM stored_objects WHERE owner_user_id=account.id),
			(SELECT COUNT(*) FROM upload_parts WHERE task_id=task.id)
			FROM upload_tasks task JOIN users account ON account.id=task.user_id
			WHERE task.id=? AND account.id=? AND task.status='COMPLETED'`, completed.ID, fixture.login.User.ID).
			Scan(&state.publishedObjectID, &state.usedBytes, &state.reservedBytes, &state.files, &state.objects, &state.receipts); err != nil {
			t.Fatal("读取原任务的发布与配额快照失败", err)
		}
		return state
	}
	before := readState()
	if before.publishedObjectID == 0 || before.usedBytes != completed.SizeBytes || before.reservedBytes != 0 ||
		before.files != 1 || before.objects != 1 || before.receipts != 2 {
		t.Fatal("重复完成前的发布快照不符", before)
	}

	// 2. 只带原任务 ID 提交完成重试，核对返回的完整终态和原条目 ID。
	var replayed dto.UploadTaskResponse
	api.request(t, ctx, http.MethodPost, taskPath+"/complete", "", nil, http.StatusOK, &replayed)
	if replayed.RequestID == "" || !reflect.DeepEqual(replayed.Data, completed) {
		t.Fatal("无正文重试未返回同一任务的已发布结果", replayed.Data)
	}

	// 3. 再读发布快照，确认文件、对象、回执和配额都没有变化。
	after := readState()
	if after != before {
		t.Fatal("重复完成改变了发布或配额快照", before, after)
	}
	t.Logf("原任务 %s 无正文重试返回 200 COMPLETED 和原条目 %s，重试前后均为 %d 个文件、%d 个对象、%d 条回执，已用 %d B、预占 %d B",
		completed.ID, *completed.ResultEntryID, after.files, after.objects, after.receipts, after.usedBytes, after.reservedBytes)
}
