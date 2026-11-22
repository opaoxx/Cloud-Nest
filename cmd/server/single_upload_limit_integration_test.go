package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

const serverSingleUploadLimitBytes = 16 * 1024 * 1024
const serverSingleUploadUserID, serverSingleUploadRootID = 7, 33

type serverSingleUploadLimitFixture struct {
	pool       *sql.DB
	store      *repository.Store
	bucket     *serverUploadContentBucket
	baseURL    string
	token      string
	httpClient *http.Client
}

func TestServerSingleUploadLimitChecksumCancellationIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	const fileSize = serverSingleUploadLimitBytes
	fixture := prepareServerSingleUploadLimitFixture(t, ctx)
	checkUnpublished := func() {
		t.Helper()
		var files uint64
		if err := fixture.pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_entries WHERE user_id=? AND entry_type='FILE'",
			serverSingleUploadUserID).Scan(&files); err != nil || files != 0 {
			t.Fatal("隔离账号出现了已发布文件", files, err)
		}
		fixture.checkPublicationEventCount(t, ctx, 0)
		for object := range fixture.bucket.client.ListObjects(ctx, fixture.bucket.name, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
			if object.Err != nil {
				t.Fatal("检查独占桶版本失败", object.Err)
			}
			t.Fatal("未发布的上传在独占桶中留下了对象版本")
		}
	}

	// 1. 创建 16 MiB 上传任务，核对全额预占。
	payload := newServerSingleUploadLimitPayload()
	actualHash := sha256.Sum256(payload)
	expectedHash := actualHash
	expectedHash[0] ^= 1
	created, pending := fixture.createTask(t, ctx, "single-limit-checksum-cancel", expectedHash)

	// 2. 提交摘要不匹配的正文，确认没有发布文件。
	var rejected dto.ErrorResponse
	fixture.requestJSON(t, ctx, http.MethodPut, created.ContentPath, "application/octet-stream", bytes.NewReader(payload),
		http.StatusUnprocessableEntity, &rejected)
	if rejected.Error.Code != "CHECKSUM_MISMATCH" {
		t.Fatal("摘要不匹配的错误码不符", rejected.Error.Code)
	}
	afterMismatch, err := fixture.store.FindUserUploadTask(ctx, pending.UserID, pending.ID)
	if err != nil || afterMismatch == nil || afterMismatch.Status != domain.UploadStatusUploading ||
		afterMismatch.ReservedBytes != fileSize || afterMismatch.StagedObjectID != pending.StagedObjectID ||
		afterMismatch.ResultEntryID != nil || afterMismatch.PublishedObjectID != nil {
		t.Fatal("摘要拒绝后任务状态、对象或预占不符", afterMismatch, err)
	}
	fixture.checkQuota(t, ctx, 0, fileSize)
	checkUnpublished()

	// 3. 经正式接口取消，等后台收尾后核对已用和预占。
	remainingLease := func() time.Duration {
		t.Helper()
		var microseconds int64
		if err := fixture.pool.QueryRowContext(ctx, `SELECT COALESCE(TIMESTAMPDIFF(MICROSECOND, UTC_TIMESTAMP(6), lease_until), 0)
			FROM upload_tasks WHERE user_id=? AND id=?`, pending.UserID, pending.ID).Scan(&microseconds); err != nil {
			t.Fatal("读取接收租约相对数据库时钟的剩余时间失败", err)
		}
		return time.Duration(microseconds) * time.Microsecond
	}
	if remaining := remainingLease(); remaining > 0 {
		requestServerUploadCancellation(t, ctx, fixture.baseURL, fixture.token, created.Task.ID, http.StatusConflict, "UPLOAD_BUSY")
		fixture.checkQuota(t, ctx, 0, fileSize)
		t.Logf("有效接收租约期间取消返回 409 UPLOAD_BUSY，等待租约自然到期（约 %s）", remaining.Round(time.Second))
		// 宿主机的等待只用于安排下一次读取，由 MySQL 确认到期。
		for remaining > 0 {
			timer := time.NewTimer(remaining + 20*time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				t.Fatal("等待上传租约到期时超时", ctx.Err())
			}
			remaining = remainingLease()
		}
		t.Log("MySQL 已确认接收租约到期")
	}
	accepted := requestServerUploadCancellation(t, ctx, fixture.baseURL, fixture.token, created.Task.ID, http.StatusAccepted, "")
	if accepted.Data.Status != "CANCELLING" || accepted.Data.ResultEntryID != nil {
		t.Fatal("正式取消未受理未发布的任务", accepted.Data)
	}
	waitContext, stopWaiting := context.WithTimeout(ctx, uploadCancellationInterval+8*time.Second)
	defer stopWaiting()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var finished *domain.UploadTask
	for finished == nil {
		current, err := fixture.store.FindUserUploadTask(waitContext, pending.UserID, pending.ID)
		if err != nil {
			t.Fatal("等待后台取消时读取任务失败", err)
		}
		switch current.Status {
		case domain.UploadStatusCancelled:
			finished = current
			continue
		case domain.UploadStatusCancelling:
		default:
			t.Fatal("后台取消进入了意外状态", current.Status)
		}
		select {
		case <-waitContext.Done():
			t.Fatal("后台取消没有在等待期限内完成", waitContext.Err())
		case <-ticker.C:
		}
	}
	if finished.ReservedBytes != 0 || finished.FileSize != fileSize || finished.StagedObjectID != pending.StagedObjectID ||
		finished.ResultEntryID != nil || finished.PublishedObjectID != nil || finished.CompletedAt == nil {
		t.Fatal("已取消任务的终态或预占不符")
	}
	queried := requestServerUploadTask(t, fixture.baseURL, fixture.token, created.Task.ID, http.StatusOK, "", "")
	if queried.Data.Status != "CANCELLED" || queried.Data.ResultEntryID != nil || queried.Data.CompletedAt == nil {
		t.Fatal("正式接口未返回对应的取消终态", queried.Data)
	}
	object, err := fixture.store.FindUserStoredObject(ctx, pending.UserID, pending.StagedObjectID)
	if err != nil || object == nil || object.Status != domain.StoredObjectStatusOrphaned || object.Bucket != fixture.bucket.name ||
		object.StorageVersionID != nil || object.VerifiedSHA256 != nil {
		t.Fatal("取消后暂存对象记录不符", err)
	}
	fixture.checkQuota(t, ctx, 0, 0)
	checkUnpublished()
	t.Logf("提交 %d 字节得到 422 CHECKSUM_MISMATCH。正式取消返回 202，后台收尾为 CANCELLED，释放 %d 字节，已用和预占均为 0，文件和桶版本均为 0",
		len(payload), fileSize)
}

func prepareServerSingleUploadLimitFixture(t *testing.T, ctx context.Context) serverSingleUploadLimitFixture {
	t.Helper()
	if os.Getenv("CLOUDNEST_SERVER_TEST") != "1" || os.Getenv("CLOUDNEST_UPLOAD_STORAGE_TEST") != "1" {
		t.Skip("同时启用启动和上传存储联测后，用独占库和桶验证 16 MiB 上传")
	}
	// 准备专用容器、独占库和桶，再装配测试会话及回环服务。
	mysqlContainer := startIsolatedWebMySQL(t, ctx)
	minioContainer := startIsolatedWebMinIO(t, ctx)
	t.Logf("单文件上传专用 MySQL %s，MinIO %s", mysqlContainer, minioContainer)
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	bucket := prepareServerUploadContentBucket(t, ctx)
	t.Setenv("CLOUDNEST_DB_NAME", databaseName)
	t.Setenv("CLOUDNEST_TEMP_DIR", t.TempDir())
	t.Setenv("CLOUDNEST_JWT_SECRET", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("u"), 32)))
	t.Setenv("CLOUDNEST_ALLOWED_ORIGINS", "http://127.0.0.1:5173")
	if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash,storage_quota) VALUES(7,'fixture_limit','上传边界测试用户','fixture_hash',?)", serverSingleUploadLimitBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(33,7,'',X'','DIR');
		INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','上传边界设备','WEB')`); err != nil {
		t.Fatal(err)
	}
	session := domain.LoginSession{ID: 17, UserID: serverSingleUploadUserID, DeviceID: 11, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
	refreshHash := sha256.Sum256([]byte("single-upload-limit-fixture"))
	if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,7,11,?,?)",
		refreshHash[:], session.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	jwtConfig, err := config.LoadServerJWTConfig()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := service.NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Issue(session)
	if err != nil {
		t.Fatal(err)
	}
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDNEST_HTTP_ADDR", address)
	runContext, stop := context.WithCancel(ctx)
	runResult := make(chan error, 1)
	var logs bytes.Buffer
	go func() { runResult <- run(runContext, logging.New(&logs)) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-runResult:
			if err != nil {
				t.Error("隔离服务退出失败", err)
			}
		case <-time.After(35 * time.Second):
			t.Error("隔离服务未在期限内停止")
		}
	})
	waitForServerIntegrationHTTP(t, ctx, address)
	httpClient := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	t.Cleanup(httpClient.CloseIdleConnections)
	return serverSingleUploadLimitFixture{
		pool: pool, store: store, bucket: bucket, baseURL: "http://" + address, token: token.Token, httpClient: httpClient,
	}
}

func (fixture serverSingleUploadLimitFixture) requestJSON(t *testing.T, ctx context.Context, method, path, contentType string, body io.Reader, status int, result any) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, method, fixture.baseURL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+fixture.token)
	request.Header.Set("Content-Type", contentType)
	response, err := fixture.httpClient.Do(request)
	if err != nil {
		t.Fatal("隔离服务请求失败", err)
	}
	defer response.Body.Close()
	if response.StatusCode != status {
		var failure dto.ErrorResponse
		_ = json.NewDecoder(response.Body).Decode(&failure)
		t.Fatal("隔离服务响应状态不符", method, response.StatusCode, failure.Error.Code)
	}
	if err := json.NewDecoder(response.Body).Decode(result); err != nil {
		t.Fatal("无法解析隔离服务响应", method, err)
	}
}

func (fixture serverSingleUploadLimitFixture) checkQuota(t *testing.T, ctx context.Context, wantUsed, wantReserved uint64) {
	t.Helper()
	var used, reserved uint64
	if err := fixture.pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", serverSingleUploadUserID).
		Scan(&used, &reserved); err != nil || used != wantUsed || reserved != wantReserved {
		t.Fatal("已用或预占空间不符", used, reserved, err)
	}
}

func (fixture serverSingleUploadLimitFixture) checkPublicationEventCount(t *testing.T, ctx context.Context, want uint64) {
	t.Helper()
	var events, sequence uint64
	if err := fixture.pool.QueryRowContext(ctx, "SELECT change_seq,(SELECT COUNT(*) FROM change_events WHERE user_id=users.id) FROM users WHERE id=?",
		serverSingleUploadUserID).Scan(&sequence, &events); err != nil || events != want || sequence != want {
		t.Fatal("隔离账号的事件数或序号不符", events, sequence, err)
	}
}

func newServerSingleUploadLimitPayload() []byte {
	payload := make([]byte, serverSingleUploadLimitBytes)
	for index := range payload {
		payload[index] = byte(index ^ (index >> 8) ^ (index >> 16))
	}
	return payload
}

func (fixture serverSingleUploadLimitFixture) createTask(t *testing.T, ctx context.Context, key string, expectedHash [sha256.Size]byte) (dto.CreateUploadData, *domain.UploadTask) {
	t.Helper()
	fixture.checkQuota(t, ctx, 0, 0)
	input := fmt.Sprintf(`{"parent_id":"%d","name":"single-limit.bin","size_bytes":%d,"sha256":"%x"}`,
		serverSingleUploadRootID, serverSingleUploadLimitBytes, expectedHash)
	created := requestServerCreateUpload(t, fixture.baseURL, fixture.token, key, input, http.StatusCreated, "", "").Data
	taskID, err := strconv.ParseUint(created.Task.ID, 10, 64)
	if err != nil || taskID == 0 || created.ContentPath != "/api/v1/uploads/"+created.Task.ID+"/content" {
		t.Fatal("创建接口未返回合法的上传任务路径")
	}
	pending, err := fixture.store.FindUserUploadTask(ctx, serverSingleUploadUserID, taskID)
	if err != nil || pending == nil || pending.Status != domain.UploadStatusUploading || pending.UploadMode != "SINGLE" ||
		pending.FileSize != serverSingleUploadLimitBytes || pending.ReservedBytes != serverSingleUploadLimitBytes ||
		pending.ExpectedSHA256 == nil || *pending.ExpectedSHA256 != expectedHash {
		t.Fatal("创建任务未保存大小、摘要或预占", err)
	}
	fixture.checkQuota(t, ctx, 0, serverSingleUploadLimitBytes)
	return created, pending
}

func TestServerSingleUploadLimitRoundTripIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	const fileSize = serverSingleUploadLimitBytes
	fixture := prepareServerSingleUploadLimitFixture(t, ctx)

	// 1. 创建上传任务，核对 16 MiB 预占。
	payload := newServerSingleUploadLimitPayload()
	sourceHash := sha256.Sum256(payload)
	created, pending := fixture.createTask(t, ctx, "single-limit-round-trip", sourceHash)
	fixture.checkPublicationEventCount(t, ctx, 0)

	// 2. 上传合成内容，等待后台校验并发布。
	var accepted dto.UploadContentAcceptedResponse
	fixture.requestJSON(t, ctx, http.MethodPut, created.ContentPath, "application/octet-stream", bytes.NewReader(payload), http.StatusAccepted, &accepted)
	if accepted.Data.TaskID != created.Task.ID || accepted.Data.Status != "VERIFYING" {
		t.Fatal("上传接收未返回当前待校验任务", accepted.Data)
	}
	waitContext, stopWaiting := context.WithTimeout(ctx, 18*time.Second)
	defer stopWaiting()
	finished := awaitServerUploadPublication(t, waitContext, fixture.store, pending.UserID, pending.ID)
	if finished.FileSize != fileSize || finished.ReservedBytes != 0 || finished.ResultEntryID == nil ||
		finished.PublishedObjectID == nil || *finished.PublishedObjectID != pending.StagedObjectID {
		t.Fatal("发布后的任务大小、预占或对象引用不符")
	}
	confirmed := requestServerUploadCompletion(t, ctx, fixture.baseURL, fixture.token, created.Task.ID, http.StatusOK, "", "")
	entryID := strconv.FormatUint(*finished.ResultEntryID, 10)
	if confirmed.Data.Status != "COMPLETED" || confirmed.Data.ResultEntryID == nil || *confirmed.Data.ResultEntryID != entryID {
		t.Fatal("完成确认未返回已发布目录项")
	}
	replayed := requestServerUploadCompletion(t, ctx, fixture.baseURL, fixture.token, created.Task.ID, http.StatusOK, "", "")
	if replayed.Data.Status != "COMPLETED" || replayed.Data.ResultEntryID == nil || *replayed.Data.ResultEntryID != entryID {
		t.Fatal("重复完成确认未返回原目录项")
	}
	fixture.checkPublicationEventCount(t, ctx, 1)
	var eventEntryID, revision, eventSize uint64
	var eventType, eventSHA256 string
	if err := fixture.pool.QueryRowContext(ctx, `SELECT entry_id,revision,event_type,
		JSON_EXTRACT(payload,'$.size_bytes'),JSON_UNQUOTE(JSON_EXTRACT(payload,'$.sha256'))
		FROM change_events WHERE user_id=?`, pending.UserID).Scan(&eventEntryID, &revision, &eventType, &eventSize, &eventSHA256); err != nil ||
		eventEntryID != *finished.ResultEntryID || revision != 1 || eventType != "FILE_PUBLISHED" ||
		eventSize != fileSize || eventSHA256 != fmt.Sprintf("%x", sourceHash) {
		t.Fatal("正式上传的发布事件与完成任务及已校验内容不符", err)
	}
	t.Logf("FILE_PUBLISHED 序号 1，结果条目 %s，版本 %d，完成接口重复调用后仍为 1 条", entryID, revision)
	object, err := fixture.store.FindUserStoredObject(ctx, pending.UserID, *finished.PublishedObjectID)
	if err != nil || object == nil || object.Status != domain.StoredObjectStatusReady || object.Bucket != fixture.bucket.name || object.FileSize != fileSize ||
		object.VerifiedSHA256 == nil || *object.VerifiedSHA256 != sourceHash || object.StorageVersionID == nil {
		t.Fatal("已发布对象的位置、大小或实测摘要不符", err)
	}
	info, err := fixture.bucket.client.StatObject(ctx, fixture.bucket.name, object.ObjectKey, minio.StatObjectOptions{VersionID: *object.StorageVersionID})
	if err != nil || info.Size != fileSize {
		t.Fatal("测试桶指定版本的大小不符", info.Size, err)
	}

	// 3. 下载并校验字节数、SHA-256 与已用空间。
	var grant dto.CreateDownloadResponse
	fixture.requestJSON(t, ctx, http.MethodPost, "/api/v1/entries/"+entryID+"/download", "application/json",
		strings.NewReader(`{"expected_revision":"1","purpose":"download"}`), http.StatusOK, &grant)
	if grant.Data.SizeBytes != fileSize || grant.Data.SHA256 != fmt.Sprintf("%x", sourceHash) ||
		grant.Data.ObjectID != strconv.FormatUint(object.ID, 10) {
		t.Fatal("下载授权未返回已校验的对象、字节数和摘要")
	}
	settings, err := config.LoadStorageConfig()
	if err != nil {
		t.Fatal(err)
	}
	downloadURL, err := url.Parse(grant.Data.URL)
	if err != nil || downloadURL.User != nil || downloadURL.Host != settings.PublicEndpointURL.Host ||
		downloadURL.Scheme != settings.PublicEndpointURL.Scheme || downloadURL.Path != "/"+fixture.bucket.name+"/"+object.ObjectKey {
		t.Fatal("签名链接未限定在本轮独占桶")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL.String(), nil)
	if err != nil {
		t.Fatal("无法构造隔离下载请求")
	}
	response, err := fixture.httpClient.Do(request)
	if err != nil {
		t.Fatal("隔离签名下载请求失败")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength != fileSize {
		t.Fatal("下载状态或声明长度不符", response.StatusCode, response.ContentLength)
	}
	downloadHash := sha256.New()
	downloaded, err := io.Copy(downloadHash, io.LimitReader(response.Body, fileSize+1))
	actualHash := downloadHash.Sum(nil)
	if err != nil || downloaded != fileSize || !bytes.Equal(actualHash, sourceHash[:]) {
		t.Fatalf("下载内容不符：字节数 %d，源 SHA-256 %x，下载 SHA-256 %x，错误 %v", downloaded, sourceHash, actualHash, err)
	}
	fixture.checkQuota(t, ctx, fileSize, 0)
	t.Logf("上传 %d 字节 SHA-256 %x，下载 %d 字节 SHA-256 %x。已用/预占为 0/0 → 0/%d → %d/0",
		len(payload), sourceHash, downloaded, actualHash, fileSize, fileSize)
}
