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
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerPartURLsStartupIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_PART_URLS_STARTUP_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_PART_URLS_STARTUP_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 在专用库和桶里创建一条已绑定存储会话的任务及两份测试身份。
	mysqlContainer := startIsolatedWebMySQL(t, ctx)
	minioContainer := startIsolatedWebMinIO(t, ctx)
	t.Setenv("CLOUDNEST_STORAGE_PROVIDER", "local_minio")
	bucket := prepareServerUploadContentBucket(t, ctx)
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := config.LoadStorageConfig()
	if err != nil {
		t.Fatal(err)
	}
	storageClient, err := storage.NewClient(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storageClient.Close)
	const key = "users/7/objects/part-urls-startup-fixture"
	const partSize = 16 * 1024 * 1024
	const fileSize = partSize + 1
	contents := make([]byte, fileSize)
	contents[partSize] = 0x33
	firstBytes, tail := contents[:partSize], contents[partSize:]
	fileDigest := sha256.Sum256(contents)
	uploadID, err := storageClient.CreateMultipartUpload(ctx, key)
	if err != nil {
		t.Fatal("无法在专用桶创建合成分片会话", err)
	}
	core := minio.Core{Client: bucket.client}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := core.AbortMultipartUpload(cleanup, bucket.name, key, uploadID); err != nil {
			t.Error("合成分片会话清理失败", minio.ToErrorResponse(err).Code)
		}
	})
	execute := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute("INSERT INTO users(id,username,display_name,password_hash,reserved_storage) VALUES(7,'part_owner','文件所有者','fixture_hash',?),(8,'part_other','另一账号','fixture_hash',0)", fileSize)
	execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES
		(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','创建设备','WEB'),
		(12,7,'28ee758a-0727-42de-ae7d-3bb33155e149','续传设备','WINDOWS'),
		(21,8,'28ee758a-0727-42de-ae7d-3bb33155e150','另一账号设备','WINDOWS')`)
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR'),(14,8,'',X'','DIR')")
	execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(23,7,?,?,?,?)",
		settings.Provider, bucket.name, key, fileSize)
	execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
		expected_sha256,file_size,part_size,part_count,status,storage_upload_id,reserved_bytes,expires_at)
		VALUES(19,7,11,23,13,'fixture.bin','MULTIPART',?,?,?,2,'UPLOADING',?,?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`,
		fileDigest[:], fileSize, partSize, uploadID, fileSize)

	// 2. 启动正式 run，使用本轮 JWT 密钥为合成会话签发访问令牌。
	api, stopServer := startBackupFixtureServer(t, ctx, databaseName)
	jwtConfig, err := config.LoadServerJWTConfig()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := service.NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	issueToken := func(userID, deviceID, sessionID uint64) string {
		t.Helper()
		expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
		refreshHash := sha256.Sum256(fmt.Appendf(nil, "part-urls-fixture-%d", sessionID))
		execute("INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,?,?)",
			sessionID, userID, deviceID, refreshHash[:], expiry)
		token, err := signer.Issue(domain.LoginSession{ID: sessionID, UserID: userID, DeviceID: deviceID, ExpiresAt: expiry})
		if err != nil {
			t.Fatal("合成会话令牌签发失败", err)
		}
		return token.Token
	}
	owner, other := api, api
	owner.accessToken, other.accessToken = issueToken(7, 12, 41), issueToken(8, 21, 42)
	readTask := func() *domain.UploadTask {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	const taskPath = "/api/v1/uploads/19/part-urls"
	const receiptsPath = "/api/v1/uploads/19/parts"
	const completionPath = "/api/v1/uploads/19/complete"
	bodyStart := fmt.Sprintf(`{"parts":[{"part_number":2,"sha256":"%x"},{"part_number":1,"sha256":"%x"}]`,
		sha256.Sum256(tail), sha256.Sum256(firstBytes))
	assertRejected := func(client backupSourceClient, method, path, body string, status int, code string) {
		t.Helper()
		var result struct {
			dto.ErrorResponse
			Data json.RawMessage `json:"data"`
		}
		client.request(t, ctx, method, path, "", strings.NewReader(body), status, &result)
		if result.Error.Code != code || result.RequestID == "" || len(result.Data) != 0 {
			t.Fatal("分片接口拒绝码、请求 ID 或结果边界不符", result.Error.Code)
		}
	}
	assertRejected(api, http.MethodPost, taskPath, bodyStart+"}", http.StatusUnauthorized, "AUTH_REQUIRED")
	assertRejected(other, http.MethodPost, taskPath, bodyStart+"}", http.StatusNotFound, "UPLOAD_NOT_FOUND")
	assertRejected(owner, http.MethodPost, "/api/v1/uploads/99999/part-urls", bodyStart+"}", http.StatusNotFound, "UPLOAD_NOT_FOUND")
	assertRejected(api, http.MethodGet, receiptsPath, "", http.StatusUnauthorized, "AUTH_REQUIRED")
	assertRejected(other, http.MethodGet, receiptsPath, "", http.StatusNotFound, "UPLOAD_NOT_FOUND")
	assertRejected(owner, http.MethodGet, "/api/v1/uploads/99999/parts", "", http.StatusNotFound, "UPLOAD_NOT_FOUND")
	assertRejected(api, http.MethodPost, completionPath, "", http.StatusUnauthorized, "AUTH_REQUIRED")
	assertRejected(other, http.MethodPost, completionPath, "", http.StatusNotFound, "UPLOAD_NOT_FOUND")

	// 3. 查询真实回执，逐次对照任务、配额和完成快照。上传前应返回空数组。
	readAccounting := func() (used, reserved, completedParts uint64) {
		t.Helper()
		err := pool.QueryRowContext(ctx, `SELECT used_storage, reserved_storage,
			(SELECT COUNT(*) FROM upload_parts WHERE task_id=19) FROM users WHERE id=7`).Scan(&used, &reserved, &completedParts)
		if err != nil {
			t.Fatal("读取合成任务配额或完成快照失败", err)
		}
		return used, reserved, completedParts
	}
	assertReceipts := func(want ...dto.UploadPartReceiptData) {
		t.Helper()
		before := readTask()
		usedBefore, reservedBefore, completedPartsBefore := readAccounting()
		var result dto.UploadPartsResponse
		owner.request(t, ctx, http.MethodGet, receiptsPath, "", nil, http.StatusOK, &result)
		if result.RequestID == "" || result.Data.PartSizeBytes != partSize || result.Data.PartCount != 2 ||
			result.Data.Parts == nil || !slices.Equal(result.Data.Parts, want) {
			t.Fatal("正式回执响应与计划或存储回执不符", result.Data)
		}
		usedAfter, reservedAfter, completedPartsAfter := readAccounting()
		if before.Status != domain.UploadStatusUploading || before.ReservedBytes != fileSize ||
			usedBefore != 0 || reservedBefore != fileSize || completedPartsBefore != 0 ||
			!reflect.DeepEqual(before, readTask()) || usedAfter != usedBefore ||
			reservedAfter != reservedBefore || completedPartsAfter != completedPartsBefore {
			t.Fatal("只读回执请求前后的任务、配额或完成快照不符")
		}
	}
	assertReceipts()

	// 4. 普通签发保留截止时间，显式续期后上传尾片并通过正式路由确认回执。
	before := readTask()
	var ordinary, renewed dto.UploadPartURLsResponse
	owner.request(t, ctx, http.MethodPost, taskPath, "", strings.NewReader(bodyStart+"}"), http.StatusOK, &ordinary)
	afterOrdinary := readTask()
	if !ordinary.Data.ExpiresAt.Equal(before.ExpiresAt) || len(ordinary.Data.Parts) != 2 ||
		!afterOrdinary.ExpiresAt.Equal(before.ExpiresAt) || !afterOrdinary.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("普通签发没有交付两片或改变了任务截止时间")
	}
	renewBody := bodyStart + fmt.Sprintf(`,"renew_if_expires_at":%q}`, before.ExpiresAt.Format(time.RFC3339Nano))
	owner.request(t, ctx, http.MethodPost, taskPath, "", strings.NewReader(renewBody), http.StatusOK, &renewed)
	committed := readTask()
	if !renewed.Data.ExpiresAt.Equal(committed.ExpiresAt) || !committed.ExpiresAt.After(before.ExpiresAt) ||
		renewed.RequestID == "" || len(renewed.Data.Parts) != 2 || renewed.Data.Parts[1].PartNumber != 1 || renewed.Data.Parts[1].SizeBytes != partSize {
		t.Fatal("续期响应未按请求顺序交付提交后的期限及两片")
	}
	assertRejected(owner, http.MethodPost, taskPath, renewBody, http.StatusConflict, "TASK_NOT_READY")
	tailPart := renewed.Data.Parts[0]
	signed, err := url.Parse(tailPart.URL)
	if err != nil || signed.Host != settings.PublicEndpointURL.Host || signed.Scheme != settings.PublicEndpointURL.Scheme ||
		signed.User != nil || signed.Path != "/"+bucket.name+"/"+key || signed.Query().Get("uploadId") != uploadID ||
		signed.Query().Get("partNumber") != "2" || tailPart.PartNumber != 2 || tailPart.SizeBytes != 1 ||
		tailPart.URLExpiresAt.After(committed.ExpiresAt) || tailPart.RequiredHeaders.ChecksumSHA256 != multipartProbeSHA256(tail) {
		t.Fatal("尾片地址未限定在本轮隔离会话或分片信息不符")
	}
	etag := putMultipartProbePart(t, ctx, owner.httpClient, signed, tailPart.RequiredHeaders.ChecksumSHA256, tail, http.StatusOK, "")
	probe := multipartProbe{core: core, bucket: bucket.name, key: key, uploadID: uploadID}
	if page := probe.readPartPage(t, ctx, 0, 2, len(tail), etag, tailPart.RequiredHeaders.ChecksumSHA256); page.IsTruncated {
		t.Fatal("本轮只应写入一个尾片")
	}
	assertReceipts(dto.UploadPartReceiptData{
		PartNumber: 2, SizeBytes: uint64(len(tail)), ETag: etag,
		ChecksumSHA256: tailPart.RequiredHeaders.ChecksumSHA256,
	})

	// 5. 通过正式完成路由核对失败释放、成功受理和无正文重试。
	firstPart := renewed.Data.Parts[1]
	firstURL, err := url.Parse(firstPart.URL)
	if err != nil || firstURL.Host != settings.PublicEndpointURL.Host || firstURL.Scheme != settings.PublicEndpointURL.Scheme ||
		firstURL.User != nil || firstURL.Path != "/"+bucket.name+"/"+key || firstURL.Query().Get("uploadId") != uploadID ||
		firstURL.Query().Get("partNumber") != "1" || firstPart.RequiredHeaders.ChecksumSHA256 != multipartProbeSHA256(firstBytes) {
		t.Fatal("普通分片地址未限定在本轮隔离会话或摘要不符")
	}
	firstETag := putMultipartProbePart(t, ctx, owner.httpClient, firstURL, firstPart.RequiredHeaders.ChecksumSHA256, firstBytes, http.StatusOK, "")
	completeBody := func(firstETag string) io.Reader {
		return strings.NewReader(fmt.Sprintf(`{"parts":[{"part_number":2,"etag":%q},{"part_number":1,"etag":%q}]}`, etag, firstETag))
	}
	var mismatch dto.ErrorResponse
	owner.request(t, ctx, http.MethodPost, completionPath, "", completeBody("EarlierReceipt"), http.StatusConflict, &mismatch)
	afterMismatch := readTask()
	mismatchUsed, mismatchReserved, mismatchRows := readAccounting()
	if mismatch.Error.Code != "UPLOAD_PARTS_MISMATCH" || mismatch.RequestID == "" ||
		!reflect.DeepEqual(mismatch.Error.Details, map[string]any{"part_numbers": []any{float64(1)}}) ||
		afterMismatch.Status != domain.UploadStatusUploading || afterMismatch.LeaseOwner != nil || afterMismatch.LeaseUntil != nil ||
		mismatchUsed != 0 || mismatchReserved != fileSize || mismatchRows != 0 {
		t.Fatal("正式完成接口的失配响应、租约或快照不符")
	}

	var completing, repeated dto.UploadTaskResponse
	owner.request(t, ctx, http.MethodPost, completionPath, "", completeBody(firstETag), http.StatusAccepted, &completing)
	acceptedTask := readTask()
	completeUsed, completeReserved, completeRows := readAccounting()
	if completing.RequestID == "" || completing.Data.ID != "19" || completing.Data.UploadMode != "MULTIPART" ||
		completing.Data.Status != "COMPLETING" || completing.Data.ResultEntryID != nil || completing.Data.CompletedAt != nil ||
		acceptedTask.Status != domain.UploadStatusCompleting || acceptedTask.LeaseOwner != nil || acceptedTask.LeaseUntil != nil ||
		acceptedTask.LeaseVersion != afterMismatch.LeaseVersion+1 || completeUsed != 0 || completeReserved != fileSize || completeRows != 2 {
		t.Fatal("正式完成接口未提交快照并交付 COMPLETING")
	}
	owner.request(t, ctx, http.MethodPost, completionPath, "", nil, http.StatusAccepted, &repeated)
	replayUsed, replayReserved, replayRows := readAccounting()
	if repeated.RequestID == "" || !reflect.DeepEqual(completing.Data, repeated.Data) || !reflect.DeepEqual(acceptedTask, readTask()) ||
		replayUsed != completeUsed || replayReserved != completeReserved || replayRows != completeRows {
		t.Fatal("无正文重试未复用同一任务快照和配额")
	}
	var matchingRows int
	err = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM upload_parts WHERE task_id=19 AND checksum_algorithm='SHA256'
		AND ((part_number=1 AND size=? AND etag=? AND checksum_value=?)
		OR (part_number=2 AND size=1 AND etag=? AND checksum_value=?))`,
		partSize, firstETag, firstPart.RequiredHeaders.ChecksumSHA256, etag, tailPart.RequiredHeaders.ChecksumSHA256).Scan(&matchingRows)
	if err != nil || matchingRows != 2 {
		t.Fatal("隔离完成快照与实际分片不符", matchingRows, err)
	}

	// 6. 单文件保持原创建、正文接收、发布和完成确认行为。
	single := []byte{0x41, 0x42, 0x43}
	var created dto.CreateUploadResponse
	owner.request(t, ctx, http.MethodPost, "/api/v1/uploads", "part-urls-single-compatibility",
		strings.NewReader(fmt.Sprintf(`{"parent_id":"13","name":"single-fixture.bin","size_bytes":%d,"sha256":"%x"}`, len(single), sha256.Sum256(single))),
		http.StatusCreated, &created)
	if created.Data.UploadMode != "SINGLE" || created.Data.ContentPath != "/api/v1/uploads/"+created.Data.Task.ID+"/content" {
		t.Fatal("单文件创建响应或正文路径变化")
	}
	singleTaskID := backupFixtureID(t, created.Data.Task.ID)
	singleCompletionPath := "/api/v1/uploads/" + created.Data.Task.ID + "/complete"
	assertRejected(owner, http.MethodPost, "/api/v1/uploads/"+created.Data.Task.ID+"/part-urls", bodyStart+"}", http.StatusConflict, "TASK_NOT_READY")
	assertRejected(owner, http.MethodGet, "/api/v1/uploads/"+created.Data.Task.ID+"/parts", "", http.StatusConflict, "TASK_NOT_READY")
	assertRejected(owner, http.MethodPost, singleCompletionPath, "", http.StatusConflict, "TASK_NOT_READY")
	var accepted dto.UploadContentAcceptedResponse
	owner.request(t, ctx, http.MethodPut, created.Data.ContentPath, "", bytes.NewReader(single), http.StatusAccepted, &accepted)
	if accepted.Data.TaskID != created.Data.Task.ID || accepted.Data.Status != "VERIFYING" {
		t.Fatal("单文件正文接收结果不符")
	}
	published := awaitServerUploadPublication(t, ctx, store, 7, singleTaskID)
	var singleCompleted dto.UploadTaskResponse
	owner.request(t, ctx, http.MethodPost, singleCompletionPath, "", nil, http.StatusOK, &singleCompleted)
	// 7. 配额与分片任务状态取同一快照，适配分片合并和校验 worker 的独立节拍。
	var used, reserved uint64
	var multipartStatus domain.UploadStatus
	if err := pool.QueryRowContext(ctx, `SELECT users.used_storage,users.reserved_storage,upload_tasks.status
		FROM users JOIN upload_tasks ON upload_tasks.user_id=users.id WHERE users.id=7 AND upload_tasks.id=19`).
		Scan(&used, &reserved, &multipartStatus); err != nil {
		t.Fatal("无法读取分片任务和配额快照", err)
	}
	wantUsed, wantReserved := uint64(len(single)), uint64(fileSize)
	switch multipartStatus {
	case domain.UploadStatusCompleting, domain.UploadStatusVerifying:
	case domain.UploadStatusCompleted:
		wantUsed += fileSize
		wantReserved = 0
	default:
		t.Fatal("分片任务未继续校验或完成发布", multipartStatus)
	}
	if published.ResultEntryID == nil || singleCompleted.Data.ResultEntryID == nil ||
		*singleCompleted.Data.ResultEntryID != fmt.Sprint(*published.ResultEntryID) || singleCompleted.Data.Status != "COMPLETED" ||
		singleCompleted.Data.UploadMode != "SINGLE" || used != wantUsed || reserved != wantReserved {
		t.Fatal("单文件发布、完成确认或分片任务配额不符", multipartStatus, used, reserved)
	}
	logs, err := stopServer()
	if err != nil || strings.Contains(logs, uploadID) || strings.Contains(logs, "X-Amz-Signature") ||
		strings.Contains(logs, owner.accessToken) || strings.Contains(logs, other.accessToken) {
		t.Fatal("隔离服务关停或签名日志保护失败", err)
	}
	t.Logf("独占 MySQL %s 库 %s，MinIO %s 桶 %s，HTTP %s。分片失配 409，受理及重试 202，快照 2 行，SINGLE 发布确认 200，分片 %s，已用 %d 字节，预占 %d 字节",
		mysqlContainer[:12], databaseName, minioContainer[:12], bucket.name, api.baseURL, multipartStatus, used, reserved)
}
