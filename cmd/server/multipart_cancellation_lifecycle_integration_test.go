package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartCancellationLifecycleIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_CANCEL_LIFECYCLE_TEST") != "1" {
		t.Skip("显式启用后创建本轮专用 MySQL、MinIO 和隔离服务")
	}
	verifyServerMultipartCleanupLifecycle(t, domain.UploadStatusCancelled, nil, false)
}

func TestServerMultipartCancellationLocalProofIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_CANCEL_LOCAL_PROOF_TEST") != "1" {
		t.Skip("显式启用后在隔离实例复验本机 MinIO 实例证明")
	}
	verifyServerMultipartCleanupLocalProof(t, domain.UploadStatusCancelled)
}

func TestServerMultipartExpirationLifecycleIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_EXPIRATION_LIFECYCLE_TEST") != "1" {
		t.Skip("显式启用后验证专用库和桶中的分片到期启停")
	}
	verifyServerMultipartCleanupLifecycle(t, domain.UploadStatusExpired, nil, false)
}

func TestServerMultipartExpirationLocalProofIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_EXPIRATION_LOCAL_PROOF_TEST") != "1" {
		t.Skip("显式启用后验证隔离服务的分片到期实例证明")
	}
	verifyServerMultipartCleanupLocalProof(t, domain.UploadStatusExpired)
}

func verifyServerMultipartCleanupLocalProof(t *testing.T, terminalStatus domain.UploadStatus) {
	t.Helper()
	t.Run("matching project", func(t *testing.T) {
		verifyServerMultipartCleanupLifecycle(t, terminalStatus, func(project string) string { return project }, true)
	})
	t.Run("missing project", func(t *testing.T) {
		verifyServerMultipartCleanupLifecycle(t, terminalStatus, func(project string) string { return project + "-missing" }, false)
	})
}

func verifyServerMultipartCleanupLifecycle(t *testing.T, terminalStatus domain.UploadStatus, selectProofProject func(string) string, wantBoundCompleted bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	pendingStatus := domain.UploadStatusCancelling
	label, completedField, interval := "分片上传取消", "cancelled", uploadCancellationInterval
	unboundStatus := domain.UploadStatusCreating
	deadlineOffsetSeconds := 3600
	if terminalStatus == domain.UploadStatusExpired {
		pendingStatus = domain.UploadStatusExpiring
		label, completedField, interval = "分片上传到期", "expired", uploadExpirationInterval
		unboundStatus = domain.UploadStatusUploading
		deadlineOffsetSeconds = -60
	}

	// 1. 在独占库和版本桶准备两条候选，覆盖未绑定与已绑定会话。
	var composeProject, proofProject string
	if selectProofProject != nil {
		composeProject = "cloudnest-cleanup-proof-" + uuid.NewString()
		proofProject = selectProofProject(composeProject)
	}
	t.Setenv("CLOUDNEST_MINIO_PROOF_COMPOSE_PROJECT", proofProject)
	mysqlID := startIsolatedWebMySQL(t, ctx)
	t.Logf("专用 MySQL %s，回环地址 %s", mysqlID[:12], os.Getenv("CLOUDNEST_DB_ADDRESS"))
	minioID := startIsolatedWebMinIOForProject(t, ctx, "128m", composeProject)
	t.Logf("专用 MinIO %s，回环地址 %s", minioID[:12], os.Getenv("CLOUDNEST_S3_ENDPOINT"))
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
	client, err := storage.NewClient(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	const userID, deviceID, authSessionID = 7, 11, 41
	const unboundTaskID, boundTaskID, unboundObjectID, boundObjectID = 19, 20, 23, 24
	const unboundKey, boundKey = "b4/lifecycle-cleanup-unbound.bin", "b4/lifecycle-cleanup-bound.bin"
	const partSize, fileSize, initialUsed, otherReserved = 16 << 20, (16 << 20) + 1, 1024, 512
	var boundVersion minio.UploadInfo
	if pendingStatus == domain.UploadStatusExpiring {
		payload := []byte("isolated unpublished object")
		boundVersion, err = bucket.client.PutObject(ctx, bucket.name, boundKey, bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{})
		if err != nil || boundVersion.VersionID == "" || boundVersion.VersionID == "null" || boundVersion.ETag == "" {
			t.Fatal("未写入隔离桶的原对象版本", err)
		}
	}
	uploadID, err := client.CreateMultipartUpload(ctx, boundKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		core := minio.Core{Client: bucket.client}
		if err := core.AbortMultipartUpload(cleanup, bucket.name, boundKey, uploadID); err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
			t.Error("隔离上传会话清理失败", minio.ToErrorResponse(err).Code)
		}
	})
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute("INSERT INTO users(id,username,display_name,password_hash,used_storage,reserved_storage) VALUES(7,'cleanup_lifecycle','收尾启停','fixture_hash',?,?)", initialUsed, 2*fileSize+otherReserved)
	execute("INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute("INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(41,7,11,UNHEX(REPEAT('41',32)),UTC_TIMESTAMP(6)+INTERVAL 1 DAY)")
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,7,?,?,?,?),(?,7,?,?,?,?)",
		unboundObjectID, settings.Provider, bucket.name, unboundKey, fileSize, boundObjectID, settings.Provider, bucket.name, boundKey, fileSize)
	execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
		expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at) VALUES
		(?,7,11,?,13,'unbound.bin','MULTIPART',NULL,UNHEX(REPEAT('ab',32)),?,?,2,?,?,TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6))),
		(?,7,11,?,13,'bound.bin','MULTIPART',?,UNHEX(REPEAT('cd',32)),?,?,2,'UPLOADING',?,TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6)))`,
		unboundTaskID, unboundObjectID, fileSize, partSize, unboundStatus, fileSize, deadlineOffsetSeconds,
		boundTaskID, boundObjectID, uploadID, fileSize, partSize, fileSize, deadlineOffsetSeconds)
	actor := domain.Actor{UserID: userID, DeviceID: deviceID, SessionID: authSessionID}
	if pendingStatus == domain.UploadStatusCancelling {
		for _, taskID := range []uint64{unboundTaskID, boundTaskID} {
			task, err := store.BeginUserMultipartUploadCancellation(ctx, actor, taskID)
			if err != nil || task == nil || task.Status != pendingStatus || task.ReservedBytes != fileSize || task.LeaseVersion != 0 {
				t.Fatal("启动前未提交合成取消意图", taskID, err)
			}
		}
	}

	// 2. 启动正式服务，在首轮汇报处暂停，读回已提交的任务与配额。
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
	t.Setenv("CLOUDNEST_JWT_SECRET", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("c"), 32)))

	reportMessage := label + "轮次结束"
	var logs bytes.Buffer
	report := &serverWorkerReportGate{
		Handler: logging.New(&logs).Handler(), reportMessage: reportMessage, reportStarted: make(chan struct{}),
		releaseReport: make(chan struct{}), httpStopped: make(chan struct{}), deadline: ctx.Done(),
	}
	releaseReport := sync.OnceFunc(func() { close(report.releaseReport) })
	runContext, stop := context.WithCancel(ctx)
	runDone := make(chan struct{})
	var runError error
	go func() {
		defer close(runDone)
		runError = run(runContext, slog.New(report))
	}()
	defer func() {
		stop()
		releaseReport()
		select {
		case <-runDone:
		case <-time.After(35 * time.Second):
			t.Error("隔离服务未在清理期限内关闭")
		}
	}()
	firstRound, stopFirstRoundWait := context.WithTimeout(ctx, interval/2)
	defer stopFirstRoundWait()
	select {
	case <-report.reportStarted:
	case <-runDone:
		t.Fatal("首轮前服务已退出", runError)
	case <-firstRound.Done():
		t.Fatal("服务启动后未及时执行首轮")
	}
	waitForServerIntegrationHTTP(t, firstRound, address)
	stopFirstRoundWait()

	finished, err := store.FindUserUploadTask(ctx, userID, unboundTaskID)
	if err != nil || finished.Status != terminalStatus || finished.ReservedBytes != 0 || finished.CompletedAt == nil || finished.LeaseVersion != 1 {
		t.Fatal("首轮未完成未绑定任务并返还预占", err)
	}
	orphaned, err := store.FindUserStoredObject(ctx, userID, unboundObjectID)
	if err != nil || orphaned.Status != domain.StoredObjectStatusOrphaned || orphaned.GCAfter == nil || orphaned.UnreferencedAt == nil ||
		!orphaned.GCAfter.Equal(orphaned.UnreferencedAt.Add(7*24*time.Hour)) {
		t.Fatal("收尾后暂存对象没有七天回收宽限", err)
	}
	bound, err := store.FindUserUploadTask(ctx, userID, boundTaskID)
	if err != nil || bound.LeaseVersion != 1 || bound.LeaseOwner != nil || bound.LeaseUntil != nil {
		t.Fatal("首轮应恰好领取一次，并在收尾或重试时释放租约", err)
	}
	wantReserved := uint64(fileSize + otherReserved)
	wantCounts := fmt.Sprintf(`"scanned":2,"%s":1,"retries_scheduled":1,"skipped":0,"candidate_errors":0`, completedField)
	if wantBoundCompleted {
		if bound.Status != terminalStatus || bound.ReservedBytes != 0 || bound.CompletedAt == nil || bound.NextAttemptAt != nil ||
			bound.AttemptCount != 0 || bound.LastErrorCode != nil {
			t.Fatal("受信复查应直接完成已绑定任务")
		}
		boundObject, err := store.FindUserStoredObject(ctx, userID, boundObjectID)
		if err != nil || boundObject.Status != domain.StoredObjectStatusOrphaned || boundObject.GCAfter == nil || boundObject.UnreferencedAt == nil ||
			!boundObject.GCAfter.Equal(boundObject.UnreferencedAt.Add(7*24*time.Hour)) {
			t.Fatal("受信复查应将原对象登记为延迟回收", err)
		}
		if pendingStatus == domain.UploadStatusExpiring {
			if boundObject.StorageVersionID == nil || *boundObject.StorageVersionID != boundVersion.VersionID ||
				boundObject.StorageETag == nil || *boundObject.StorageETag != boundVersion.ETag {
				t.Fatal("到期终态未登记原版本与 ETag")
			}
			if candidates, err := store.ListMultipartUploadExpirationCandidates(ctx, 100); err != nil || len(candidates) != 0 {
				t.Fatal("已到期任务仍在下一轮候选中", err)
			}
		} else {
			// 取消请求的重复受理复用刚才的终态。
			replayed, err := store.BeginUserMultipartUploadCancellation(ctx, actor, boundTaskID)
			if err != nil || replayed.Status != domain.UploadStatusCancelled || replayed.LeaseVersion != bound.LeaseVersion ||
				replayed.ReservedBytes != 0 || replayed.CompletedAt == nil || !replayed.CompletedAt.Equal(*bound.CompletedAt) {
				t.Fatal("重复受理改变了原取消结果", err)
			}
			if candidates, err := store.ListMultipartUploadCancellationCandidates(ctx, 100); err != nil || len(candidates) != 0 {
				t.Fatal("已取消任务仍在下一轮候选中", err)
			}
		}
		wantReserved = otherReserved
		wantCounts = fmt.Sprintf(`"scanned":2,"%s":2,"retries_scheduled":0,"skipped":0,"candidate_errors":0`, completedField)
	} else if bound.Status != pendingStatus || bound.ReservedBytes != fileSize || bound.CompletedAt != nil ||
		bound.AttemptCount != 1 || bound.NextAttemptAt == nil || bound.LastErrorCode == nil || *bound.LastErrorCode != "STORAGE_UNAVAILABLE" {
		t.Fatal("缺少受信证明时，原绑定任务应保留预占并重试")
	}
	if _, err := client.ListUploadPartsPage(ctx, boundKey, uploadID, 0, 1); !errors.Is(err, domain.ErrUploadPartSessionMissing) {
		t.Fatal("首轮未中止隔离桶内的原会话", err)
	}
	if pendingStatus == domain.UploadStatusExpiring {
		stored, err := bucket.client.StatObject(ctx, bucket.name, boundKey, minio.StatObjectOptions{VersionID: boundVersion.VersionID})
		if err != nil || stored.VersionID != boundVersion.VersionID || stored.ETag != boundVersion.ETag {
			t.Fatal("分片收尾后原对象版本未能读取", err)
		}
	}
	var used, reserved uint64
	if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", userID).Scan(&used, &reserved); err != nil ||
		used != initialUsed || reserved != wantReserved {
		t.Fatal("首轮结算与所选证明不符", used, reserved, err)
	}

	// 3. 先停 HTTP，确认服务仍在等 worker 汇报，然后核对连接释放顺序。
	stop()
	select {
	case <-report.httpStopped:
	case <-runDone:
		t.Fatal("HTTP 停止前服务已退出", runError)
	case <-ctx.Done():
		t.Fatal("HTTP 未在期限内停止")
	}
	select {
	case <-runDone:
		t.Fatal("worker 仍在汇报时服务提前退出", runError)
	case <-time.After(100 * time.Millisecond):
	}
	releaseReport()
	select {
	case <-runDone:
		if runError != nil {
			t.Fatal("隔离服务关停失败", runError)
		}
	case <-ctx.Done():
		t.Fatal("worker 完成汇报后服务未退出")
	}
	logged := logs.String()
	previous := -1
	for _, message := range []string{"HTTP 服务已停止", reportMessage, label + " worker 已停止", "MinIO 空闲连接已释放", "MySQL 连接池已关闭"} {
		position := strings.Index(logged, message)
		if position <= previous {
			t.Fatal("关停日志缺失或次序不符", message)
		}
		previous = position
	}
	if strings.Count(logged, reportMessage) != 1 || !strings.Contains(logged, wantCounts) ||
		!strings.Contains(logged, `"interval_ms":30000,"batch_limit":100`) || strings.Contains(logged, uploadID) {
		t.Fatal("首轮汇报数量、调度参数或脱敏检查不符")
	}
	t.Logf("HTTP %s，首轮任务 %d 为 %s，已绑定任务 %d 为 %s，已用 %d B、预占 %d B。HTTP 停止后等 worker 汇报和退出，再关闭 MinIO 和 MySQL",
		address, unboundTaskID, terminalStatus, boundTaskID, bound.Status, used, reserved)
}
