package main

import (
	"bytes"
	"context"
	"crypto/sha256"
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

func TestServerSingleUploadLimitRoundTripIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_SERVER_TEST") != "1" || os.Getenv("CLOUDNEST_UPLOAD_STORAGE_TEST") != "1" {
		t.Skip("同时启用启动和上传存储联测后，用独占库和桶验证 16 MiB 往返")
	}
	// 准备独占库、独占桶、测试会话及隔离服务地址。
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	const fileSize = 16 * 1024 * 1024
	const userID, rootID = 7, 33
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	bucket := prepareServerUploadContentBucket(t, ctx)
	t.Setenv("CLOUDNEST_DB_NAME", databaseName)
	t.Setenv("CLOUDNEST_TEMP_DIR", t.TempDir())
	t.Setenv("CLOUDNEST_JWT_SECRET", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("u"), 32)))
	t.Setenv("CLOUDNEST_ALLOWED_ORIGINS", "http://127.0.0.1:5173")
	if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash,storage_quota) VALUES(7,'fixture_limit','上传边界测试用户','fixture_hash',?)", fileSize); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(33,7,'',X'','DIR');
		INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','上传边界设备','WEB')`); err != nil {
		t.Fatal(err)
	}
	session := domain.LoginSession{ID: 17, UserID: userID, DeviceID: 11, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
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
	defer func() {
		stop()
		select {
		case err := <-runResult:
			if err != nil {
				t.Error("隔离服务退出失败", err)
			}
		case <-time.After(35 * time.Second):
			t.Error("隔离服务未在期限内停止")
		}
	}()
	waitForServerIntegrationHTTP(t, ctx, address)
	baseURL := "http://" + address
	httpClient := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	defer httpClient.CloseIdleConnections()
	requestJSON := func(method, path, contentType string, body io.Reader, status int, result any) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+token.Token)
		request.Header.Set("Content-Type", contentType)
		response, err := httpClient.Do(request)
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
	checkQuota := func(wantUsed, wantReserved uint64) {
		t.Helper()
		var used, reserved uint64
		if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", userID).
			Scan(&used, &reserved); err != nil || used != wantUsed || reserved != wantReserved {
			t.Fatal("已用或预占空间不符", used, reserved, err)
		}
	}

	// 1. 创建上传任务，核对 16 MiB 预占。
	payload := make([]byte, fileSize)
	for index := range payload {
		payload[index] = byte(index ^ (index >> 8) ^ (index >> 16))
	}
	sourceHash := sha256.Sum256(payload)
	checkQuota(0, 0)
	input := fmt.Sprintf(`{"parent_id":"%d","name":"single-limit.bin","size_bytes":%d,"sha256":"%x"}`, rootID, len(payload), sourceHash)
	created := requestServerCreateUpload(t, baseURL, token.Token, "single-limit-round-trip", input, http.StatusCreated, "", "").Data
	taskID, err := strconv.ParseUint(created.Task.ID, 10, 64)
	if err != nil || taskID == 0 || created.ContentPath != "/api/v1/uploads/"+created.Task.ID+"/content" {
		t.Fatal("创建接口未返回合法的上传任务路径")
	}
	pending, err := store.FindUserUploadTask(ctx, userID, taskID)
	if err != nil || pending == nil || pending.Status != domain.UploadStatusUploading || pending.UploadMode != "SINGLE" ||
		pending.FileSize != fileSize || pending.ReservedBytes != fileSize ||
		pending.ExpectedSHA256 == nil || *pending.ExpectedSHA256 != sourceHash {
		t.Fatal("创建任务未保存大小、摘要或预占", err)
	}
	checkQuota(0, fileSize)

	// 2. 上传合成内容，等待后台校验并发布。
	var accepted dto.UploadContentAcceptedResponse
	requestJSON(http.MethodPut, created.ContentPath, "application/octet-stream", bytes.NewReader(payload), http.StatusAccepted, &accepted)
	if accepted.Data.TaskID != created.Task.ID || accepted.Data.Status != "VERIFYING" {
		t.Fatal("上传接收未返回当前待校验任务", accepted.Data)
	}
	waitContext, stopWaiting := context.WithTimeout(ctx, 18*time.Second)
	defer stopWaiting()
	finished := awaitServerUploadPublication(t, waitContext, store, userID, taskID)
	if finished.FileSize != fileSize || finished.ReservedBytes != 0 || finished.ResultEntryID == nil ||
		finished.PublishedObjectID == nil || *finished.PublishedObjectID != pending.StagedObjectID {
		t.Fatal("发布后的任务大小、预占或对象引用不符")
	}
	confirmed := requestServerUploadCompletion(t, ctx, baseURL, token.Token, created.Task.ID, http.StatusOK, "", "")
	entryID := strconv.FormatUint(*finished.ResultEntryID, 10)
	if confirmed.Data.Status != "COMPLETED" || confirmed.Data.ResultEntryID == nil || *confirmed.Data.ResultEntryID != entryID {
		t.Fatal("完成确认未返回已发布目录项")
	}
	object, err := store.FindUserStoredObject(ctx, userID, *finished.PublishedObjectID)
	if err != nil || object == nil || object.Status != domain.StoredObjectStatusReady || object.Bucket != bucket.name || object.FileSize != fileSize ||
		object.VerifiedSHA256 == nil || *object.VerifiedSHA256 != sourceHash || object.StorageVersionID == nil {
		t.Fatal("已发布对象的位置、大小或实测摘要不符", err)
	}
	info, err := bucket.client.StatObject(ctx, bucket.name, object.ObjectKey, minio.StatObjectOptions{VersionID: *object.StorageVersionID})
	if err != nil || info.Size != fileSize {
		t.Fatal("测试桶指定版本的大小不符", info.Size, err)
	}

	// 3. 下载并校验字节数、SHA-256 与已用空间。
	var grant dto.CreateDownloadResponse
	requestJSON(http.MethodPost, "/api/v1/entries/"+entryID+"/download", "application/json",
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
		downloadURL.Scheme != settings.PublicEndpointURL.Scheme || downloadURL.Path != "/"+bucket.name+"/"+object.ObjectKey {
		t.Fatal("签名链接未限定在本轮独占桶")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL.String(), nil)
	if err != nil {
		t.Fatal("无法构造隔离下载请求")
	}
	response, err := httpClient.Do(request)
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
	checkQuota(fileSize, 0)
	t.Logf("上传 %d 字节 SHA-256 %x，下载 %d 字节 SHA-256 %x。已用/预占为 0/0 → 0/%d → %d/0",
		len(payload), sourceHash, downloaded, actualHash, fileSize, fileSize)
}
