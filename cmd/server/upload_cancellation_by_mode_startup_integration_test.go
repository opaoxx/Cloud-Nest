package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerUploadCancellationByModeStartupIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_UPLOAD_CANCEL_BY_MODE_STARTUP_TEST") != "1" {
		t.Skip("显式启用后创建本轮专用 MySQL、MinIO 和隔离 HTTP 服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	project := "cloudnest-cancel-http-" + uuid.NewString()
	t.Setenv("CLOUDNEST_MINIO_PROOF_COMPOSE_PROJECT", project)
	t.Setenv("CLOUDNEST_STORAGE_PROVIDER", "local_minio")
	t.Setenv("CLOUDNEST_ALLOWED_ORIGINS", "http://127.0.0.1:5173")
	mysqlID := startIsolatedWebMySQL(t, ctx)
	t.Logf("专用 MySQL %s，回环地址 %s", mysqlID[:12], os.Getenv("CLOUDNEST_DB_ADDRESS"))
	minioID := startIsolatedWebMinIOForProject(t, ctx, "128m", project)
	t.Logf("专用 MinIO %s，回环地址 %s", minioID[:12], os.Getenv("CLOUDNEST_S3_ENDPOINT"))
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
	client, err := storage.NewClient(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	// 1. 准备尚未取消的两种上传任务、原分片会话及一条保留预占的对照任务。
	const userID, foreignID, singleTaskID, multipartTaskID, retainedTaskID = 7, 8, 19, 20, 21
	const singleSize, partSize, multipartSize, retainedSize = 9, 16 << 20, (16 << 20) + 1, 512
	const multipartKey = "b4/cancel-http-multipart.bin"
	uploadID, err := client.CreateMultipartUpload(ctx, multipartKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		core := minio.Core{Client: bucket.client}
		if err := core.AbortMultipartUpload(cleanup, bucket.name, multipartKey, uploadID); err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
			t.Error("专用会话清理失败", minio.ToErrorResponse(err).Code)
		}
	})
	if _, err := client.ListUploadPartsPage(ctx, multipartKey, uploadID, 0, 1); err != nil {
		t.Fatal("取消之前原分片会话应当可查询", err)
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	execute(`INSERT INTO users(id,username,display_name,password_hash,reserved_storage) VALUES
		(7,'cancel_http_owner','取消接口账号','fixture_hash',?),(8,'cancel_http_other','隔离对照账号','fixture_hash',0)`, singleSize+multipartSize+retainedSize)
	execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES
		(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB'),(12,8,'38ee758a-0727-42de-ae7d-3bb33155e148','other','WEB')`)
	execute(`INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES
		(41,7,11,UNHEX(REPEAT('41',32)),?),(42,8,12,UNHEX(REPEAT('42',32)),?)`, expires, expires)
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES
		(23,7,?,?,'b4/cancel-http-single.bin',?),(24,7,?,?,?,?),(25,7,?,?,'b4/cancel-http-retained.bin',?)`,
		settings.Provider, bucket.name, singleSize, settings.Provider, bucket.name, multipartKey, multipartSize, settings.Provider, bucket.name, retainedSize)
	execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
		expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at) VALUES
		(19,7,11,23,13,'single.bin','SINGLE',NULL,NULL,?,NULL,NULL,'UPLOADING',?,?),
		(20,7,11,24,13,'multipart.bin','MULTIPART',?,UNHEX(REPEAT('ab',32)),?,?,2,'UPLOADING',?,?),
		(21,7,11,25,13,'retained.bin','SINGLE',NULL,NULL,?,NULL,NULL,'UPLOADING',?,?)`,
		singleSize, singleSize, expires, uploadID, multipartSize, partSize, multipartSize, expires, retainedSize, retainedSize, expires)
	readTask := func(taskID uint64) *domain.UploadTask {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, userID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	single, multipart, retained := readTask(singleTaskID), readTask(multipartTaskID), readTask(retainedTaskID)
	assertQuota := func(wantReserved uint64) {
		t.Helper()
		var used, reserved, files uint64
		if err := pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,
			(SELECT COUNT(*) FROM file_entries WHERE user_id=users.id AND entry_type='FILE') FROM users WHERE id=?`, userID).
			Scan(&used, &reserved, &files); err != nil || used != 0 || reserved != wantReserved || files != 0 {
			t.Fatal("取消后的用量或文件数不符", used, reserved, files, wantReserved, err)
		}
	}
	assertQuota(singleSize + multipartSize + retainedSize)

	// 2. 运行正式服务，验证 Bearer 会话、用户归属和两种模式的 202 受理。
	api, _ := startBackupFixtureServer(t, ctx, databaseName)
	jwtConfig, err := config.LoadServerJWTConfig()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := service.NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Issue(domain.LoginSession{ID: 41, UserID: userID, DeviceID: 11, ExpiresAt: expires})
	if err != nil {
		t.Fatal(err)
	}
	other, err := signer.Issue(domain.LoginSession{ID: 42, UserID: foreignID, DeviceID: 12, ExpiresAt: expires})
	if err != nil {
		t.Fatal(err)
	}
	multipartID := strconv.FormatUint(multipart.ID, 10)
	requestServerUploadCancellation(t, ctx, api.baseURL, "", multipartID, http.StatusUnauthorized, "AUTH_REQUIRED")
	requestServerUploadCancellation(t, ctx, api.baseURL, other.Token, multipartID, http.StatusNotFound, "UPLOAD_NOT_FOUND")
	if !reflect.DeepEqual(multipart, readTask(multipart.ID)) {
		t.Fatal("未授权的取消改变了分片任务")
	}
	for _, task := range []*domain.UploadTask{single, multipart} {
		taskID := strconv.FormatUint(task.ID, 10)
		accepted := requestServerUploadCancellation(t, ctx, api.baseURL, token.Token, taskID, http.StatusAccepted, "")
		if accepted.Data.UploadMode != task.UploadMode || accepted.Data.Status != "CANCELLING" ||
			accepted.Data.CompletedAt != nil || accepted.Data.ResultEntryID != nil {
			t.Fatal("未返回已提交的真实取消受理状态", taskID)
		}
	}

	// 3. 等待正式 worker 写入终态，核对会话、对象、HTTP 重放和配额。
	for _, task := range []*domain.UploadTask{single, multipart} {
		finished := awaitBackupTargetCancellation(t, ctx, pool, task)
		taskID := strconv.FormatUint(task.ID, 10)
		queried := requestServerUploadTask(t, api.baseURL, token.Token, taskID, http.StatusOK, "", "")
		replayed := requestServerUploadCancellation(t, ctx, api.baseURL, token.Token, taskID, http.StatusOK, "")
		want, err := dto.NewUploadTaskData(finished)
		if err != nil || !reflect.DeepEqual(queried.Data, *want) || !reflect.DeepEqual(replayed.Data, *want) || queried.RequestID == replayed.RequestID {
			t.Fatal("查询或重复取消未返回同一终态", taskID, err)
		}
		object, err := store.FindUserStoredObject(ctx, userID, task.StagedObjectID)
		if err != nil || object.Status != domain.StoredObjectStatusOrphaned || object.UnreferencedAt == nil || object.GCAfter == nil ||
			!object.GCAfter.Equal(object.UnreferencedAt.Add(7*24*time.Hour)) {
			t.Fatal("取消未登记暂存对象回收宽限", taskID, err)
		}
	}
	if _, err := client.ListUploadPartsPage(ctx, multipartKey, uploadID, 0, 1); !errors.Is(err, domain.ErrUploadPartSessionMissing) {
		t.Fatal("正式取消没有中止原分片会话", err)
	}
	assertQuota(retainedSize)
	if !reflect.DeepEqual(retained, readTask(retainedTaskID)) {
		t.Fatal("取消改变了其他上传任务")
	}
	api.accessToken = token.Token
	var profile dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	if profile.Data.UsedBytes != 0 || profile.Data.ReservedBytes != retainedSize {
		t.Fatal("本人资料与取消后的账目不符")
	}
	t.Logf("HTTP %s，SINGLE 和 MULTIPART 均从 202 CANCELLING 到 200 CANCELLED，返还 %d B，只保留对照任务 %d B 预占", api.baseURL, singleSize+multipartSize, retainedSize)
}
