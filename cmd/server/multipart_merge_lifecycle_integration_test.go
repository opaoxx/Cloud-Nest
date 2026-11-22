package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartMergeLifecycleIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_MERGE_LIFECYCLE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_MERGE_LIFECYCLE_TEST=1 后启动隔离服务")
	}

	// 1. 在独占库和版本化桶中准备合成分片，事务提交完成意图后才启动正式服务。
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	mysqlContainer := startIsolatedWebMySQL(t, ctx)
	minioContainer := startIsolatedWebMinIO(t, ctx)
	t.Logf("专用 MySQL %s，MinIO %s，存储回环地址 %s", mysqlContainer[:12], minioContainer[:12], os.Getenv("CLOUDNEST_S3_ENDPOINT"))
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
	const partSize = 16 << 20
	const fileSize = partSize + 1
	const key = "users/7/objects/multipart-merge-lifecycle-fixture"
	uploadID, err := storageClient.CreateMultipartUpload(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	core := minio.Core{Client: bucket.client}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := core.AbortMultipartUpload(cleanup, bucket.name, key, uploadID); err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
			t.Error("清理独占桶内的会话失败", minio.ToErrorResponse(err).Code)
		}
	})
	probe := multipartProbe{core: core, bucket: bucket.name, key: key, uploadID: uploadID, endpoint: settings.EndpointURL}
	httpClient := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(httpClient.CloseIdleConnections)
	digest := sha256.New()
	for index, payload := range [][]byte{bytes.Repeat([]byte{0x22}, partSize), {0x33}} {
		digest.Write(payload)
		checksum := multipartProbeSHA256(payload)
		putMultipartProbePart(t, ctx, httpClient, probe.sign(t, ctx, index+1, len(payload), checksum), checksum, payload, http.StatusOK, "")
	}
	parts, err := storageClient.ListUploadParts(ctx, key, uploadID, 1)
	if err != nil || len(parts) != 2 {
		t.Fatal("未取得两个合成分片回执", err)
	}
	execute := func(statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	execute("INSERT INTO users(id,username,display_name,password_hash,reserved_storage) VALUES(7,'multipart_merge_fixture','合并夹具','fixture_hash',?)", fileSize)
	execute("INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute("INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(41,7,11,UNHEX(REPEAT('41',32)),UTC_TIMESTAMP(6)+INTERVAL 1 DAY)")
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(23,7,?,?,?,?)", settings.Provider, bucket.name, key, fileSize)
	execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
		expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
		VALUES(19,7,11,23,13,'fixture.bin','MULTIPART',?,?,?,16777216,2,'UPLOADING',?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`,
		uploadID, digest.Sum(nil), fileSize, fileSize)
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 41}
	uploading, err := store.FindUserUploadTask(ctx, actor.UserID, 19)
	if err != nil {
		t.Fatal(err)
	}
	requestLease, err := store.ClaimUserMultipartCompletionLease(ctx, actor, uploading, "fixture-check", service.ResolveMultipartUploadCompletionStatus)
	if err != nil {
		t.Fatal(err)
	}
	completing, err := store.CommitUserMultipartCompletion(ctx, actor, requestLease, parts)
	if err != nil || completing.Status != domain.UploadStatusCompleting || completing.LeaseOwner != nil {
		t.Fatal("启动前未提交完成意图并释放核对租约", err)
	}

	// 2. 运行正式启停入口，先在首轮分片校验启动前核对合并结果。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDNEST_HTTP_ADDR", address)
	t.Setenv("CLOUDNEST_DB_NAME", databaseName)
	t.Setenv("CLOUDNEST_TEMP_DIR", t.TempDir())
	t.Setenv("CLOUDNEST_JWT_SECRET", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("m"), 32)))

	var logs bytes.Buffer
	const mergeReportMessage, verifyReportMessage = "分片合并轮次结束", "分片校验轮次结束"
	newReportGate := func(next slog.Handler, message string) *serverWorkerReportGate {
		return &serverWorkerReportGate{
			Handler: next, reportMessage: message, reportStarted: make(chan struct{}),
			releaseReport: make(chan struct{}), httpStopped: make(chan struct{}), deadline: ctx.Done(),
		}
	}
	mergeReport := newReportGate(logging.New(&logs).Handler(), mergeReportMessage)
	verifyStart := newReportGate(mergeReport, "分片校验 worker 已启动")
	verifyReport := newReportGate(verifyStart, verifyReportMessage)
	releaseMerge := sync.OnceFunc(func() { close(mergeReport.releaseReport) })
	releaseVerifyStart := sync.OnceFunc(func() { close(verifyStart.releaseReport) })
	releaseVerifyReport := sync.OnceFunc(func() { close(verifyReport.releaseReport) })
	runContext, stop := context.WithCancel(ctx)
	runDone := make(chan struct{})
	var runError error
	go func() {
		defer close(runDone)
		runError = run(runContext, slog.New(verifyReport))
	}()
	defer func() {
		stop()
		releaseMerge()
		releaseVerifyStart()
		releaseVerifyReport()
		select {
		case <-runDone:
		case <-time.After(35 * time.Second):
			t.Error("隔离服务未在清理期限内关闭")
		}
	}()
	waitForGate := func(gate *serverWorkerReportGate, waitContext context.Context) {
		t.Helper()
		select {
		case <-gate.reportStarted:
		case <-runDone:
			t.Fatal("服务在预期事件前退出", gate.reportMessage, runError)
		case <-waitContext.Done():
			t.Fatal("未在期限内到达预期事件", gate.reportMessage)
		}
	}
	waitForGate(verifyStart, ctx)
	waitForGate(mergeReport, ctx)
	task, err := store.FindUserUploadTask(ctx, actor.UserID, completing.ID)
	if err != nil || task.Status != domain.UploadStatusVerifying || task.StagedObjectID != completing.StagedObjectID ||
		task.LeaseVersion != completing.LeaseVersion+1 || task.LeaseOwner != nil || task.LeaseUntil != nil ||
		task.NextAttemptAt != nil || task.ReservedBytes != fileSize || task.ResultEntryID != nil {
		t.Fatal("首轮未提交预期的待校验任务", err)
	}
	object, err := store.FindUserStoredObject(ctx, actor.UserID, task.StagedObjectID)
	if err != nil || object.Status != domain.StoredObjectStatusVerifying || object.ObjectKey != key ||
		object.StorageVersionID == nil || *object.StorageVersionID == "" || object.StorageETag == nil || object.VerifiedAt != nil {
		t.Fatal("首轮未保存待校验对象的版本和 ETag", err)
	}
	info, err := bucket.client.StatObject(ctx, bucket.name, key, minio.StatObjectOptions{VersionID: *object.StorageVersionID})
	if err != nil || info.VersionID != *object.StorageVersionID || info.Size != fileSize || info.ETag != *object.StorageETag {
		t.Fatal("已提交的版本、大小或 ETag 与 MinIO 不符", minio.ToErrorResponse(err).Code)
	}
	var reserved, used, changeSequence, eventCount uint64
	if err := pool.QueryRowContext(ctx, `SELECT reserved_storage,used_storage,change_seq,
		(SELECT COUNT(*) FROM change_events WHERE user_id=users.id) FROM users WHERE id=?`, actor.UserID).
		Scan(&reserved, &used, &changeSequence, &eventCount); err != nil ||
		reserved != fileSize || used != 0 || changeSequence != 0 || eventCount != 0 {
		t.Fatal("合并待校验时应保留全额预占，已用、序号和事件数仍为零", reserved, used, changeSequence, eventCount, err)
	}
	t.Log("合并对象已保存且任务为 VERIFYING，发布事件数及账号序号均为 0")
	releaseMerge()

	// 3. 放行校验首轮，核对原版本发布、目录项和只结算一次的配额。
	firstRoundContext, stopFirstRoundWait := context.WithTimeout(ctx, uploadVerificationInterval)
	releaseVerifyStart()
	waitForGate(verifyReport, firstRoundContext)
	stopFirstRoundWait()
	waitForServerIntegrationHTTP(t, ctx, address)
	published, err := store.FindUserUploadTask(ctx, actor.UserID, task.ID)
	if err != nil || published.Status != domain.UploadStatusCompleted || published.LeaseVersion != task.LeaseVersion+1 ||
		published.LeaseOwner != nil || published.LeaseUntil != nil || published.NextAttemptAt != nil || published.ReservedBytes != 0 ||
		published.PublishedObjectID == nil || *published.PublishedObjectID != object.ID || published.ResultEntryID == nil {
		t.Fatal("分片校验首轮未提交预期的发布结果", published, err)
	}
	verifiedObject, err := store.FindUserStoredObject(ctx, actor.UserID, object.ID)
	if err != nil || verifiedObject.Status != domain.StoredObjectStatusReady || verifiedObject.VerifiedSHA256 == nil ||
		!bytes.Equal(verifiedObject.VerifiedSHA256[:], digest.Sum(nil)) || verifiedObject.FileSize != fileSize ||
		verifiedObject.StorageVersionID == nil || *verifiedObject.StorageVersionID != info.VersionID ||
		verifiedObject.StorageETag == nil || *verifiedObject.StorageETag != info.ETag {
		t.Fatal("分片校验未用合并保存的原版本发布", verifiedObject, err)
	}
	entry, err := store.FindUserFileEntry(ctx, actor.UserID, *published.ResultEntryID)
	if err != nil || entry.Status != "ACTIVE" || entry.EntryType != "FILE" || entry.ParentID == nil ||
		*entry.ParentID != task.TargetParentID || entry.Name != task.RequestedName || entry.FileSize != fileSize {
		t.Fatal("发布后的目录项不符", entry, err)
	}
	var files int
	if err := pool.QueryRowContext(ctx, `SELECT reserved_storage,used_storage,
		(SELECT COUNT(*) FROM file_entries WHERE user_id=users.id AND entry_type='FILE') FROM users WHERE id=?`, actor.UserID).
		Scan(&reserved, &used, &files); err != nil || reserved != 0 || used != fileSize || files != 1 {
		t.Fatal("校验发布后必须恰好结算一次配额", reserved, used, files, err)
	}

	// 4. 先停 HTTP，确认服务等待分片校验汇报，再核对连接释放顺序。
	stop()
	select {
	case <-verifyReport.httpStopped:
	case <-runDone:
		t.Fatal("HTTP 正常停止前服务已退出", runError)
	case <-ctx.Done():
		t.Fatal("HTTP 未在期限内停止")
	}
	select {
	case <-runDone:
		t.Fatal("分片校验 worker 仍在汇报，服务已提前退出", runError)
	case <-time.After(100 * time.Millisecond):
	}
	releaseVerifyReport()
	select {
	case <-runDone:
		if runError != nil {
			t.Fatal("隔离服务关停失败", runError)
		}
	case <-ctx.Done():
		t.Fatal("完成汇报后服务未退出")
	}
	logged := logs.String()
	previous := -1
	for _, message := range []string{"HTTP 服务已停止", verifyReportMessage, "分片校验 worker 已停止", "MinIO 空闲连接已释放", "MySQL 连接池已关闭"} {
		position := strings.Index(logged, message)
		if position <= previous {
			t.Fatal("关停日志缺失或顺序不符", message)
		}
		previous = position
	}
	mergeStopped := strings.Index(logged, "分片合并 worker 已停止")
	if mergeStopped < 0 || mergeStopped >= strings.Index(logged, "MinIO 空闲连接已释放") {
		t.Fatal("分片合并 worker 未在释放连接前停止")
	}
	if strings.Count(logged, mergeReportMessage) != 1 || strings.Count(logged, verifyReportMessage) != 1 ||
		!strings.Contains(logged, `"scanned":1,"verifying":1,"reopened":0,"failed":0,"retries_scheduled":0,"skipped":0,"candidate_errors":0`) ||
		!strings.Contains(logged, `"scanned":1,"published":1,"retries_scheduled":0,"candidate_errors":0`) ||
		strings.Contains(logged, uploadID) {
		t.Fatal("合并或分片校验首轮汇报计数、脱敏不符")
	}
	t.Logf("HTTP %s，任务 %d 首轮 %s → %s → %s，条目 %d，版本 %s，SHA-256 %x，%d 字节预占，%d 字节已用。先等分片校验 worker，再释放 MinIO 和 MySQL",
		address, task.ID, completing.Status, task.Status, published.Status, entry.ID, info.VersionID, *verifiedObject.VerifiedSHA256, reserved, used)
}
