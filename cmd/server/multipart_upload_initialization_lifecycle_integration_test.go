package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerMultipartUploadInitializationLifecycleIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_INITIALIZATION_LIFECYCLE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_INITIALIZATION_LIFECYCLE_TEST=1 后启动隔离服务")
	}

	// 1. 在独占数据库和桶中准备一条已预占、未绑定的分片任务。
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	mysqlContainer := startIsolatedWebMySQL(t, ctx)
	minioContainer := startIsolatedWebMinIO(t, ctx)
	t.Setenv("CLOUDNEST_STORAGE_PROVIDER", "local_minio")
	bucket := prepareServerUploadContentBucket(t, ctx)
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	const size = 16*1024*1024 + 1
	const key = "users/7/objects/multipart-lifecycle-fixture"
	execute := func(statement string, values ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, values...); err != nil {
			t.Fatal(err)
		}
	}
	execute("INSERT INTO users(id,username,display_name,password_hash,reserved_storage) VALUES(7,'multipart_fixture','分片夹具','fixture_hash',?)", size)
	execute("INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(23,7,'local_minio',?,?,?)", bucket.name, key, size)
	execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
		expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
		VALUES(19,7,11,23,13,'fixture.bin','MULTIPART',UNHEX(REPEAT('ab',32)),?,16777216,2,'CREATING',?,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`, size, size)

	// 2. 启动正式 run，在首轮汇报暂停时核对已提交的任务和存储会话。
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
	const reportMessage = "分片上传初始化轮次结束"
	gate := &serverWorkerReportGate{
		Handler: logging.New(&logs).Handler(), reportMessage: reportMessage, reportStarted: make(chan struct{}),
		releaseReport: make(chan struct{}), httpStopped: make(chan struct{}), deadline: ctx.Done(),
	}
	releaseReport := sync.OnceFunc(func() { close(gate.releaseReport) })
	runContext, stop := context.WithCancel(ctx)
	runDone := make(chan struct{})
	var runError error
	go func() {
		defer close(runDone)
		runError = run(runContext, slog.New(gate))
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
	select {
	case <-gate.reportStarted:
	case <-runDone:
		t.Fatal("服务在初始化首轮前退出", runError)
	case <-ctx.Done():
		t.Fatal("初始化首轮未在期限内汇报")
	}
	waitForServerIntegrationHTTP(t, ctx, address)
	task, err := store.FindUserUploadTask(ctx, 7, 19)
	if err != nil || task.StorageUploadID == nil {
		t.Fatal("首轮未绑定会话 ID", err)
	}
	uploadID := *task.StorageUploadID
	core := minio.Core{Client: bucket.client}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := core.AbortMultipartUpload(cleanup, bucket.name, key, uploadID); err != nil {
			t.Error("清理独占桶内的会话失败", minio.ToErrorResponse(err).Code)
		}
	})
	if task.Status != domain.UploadStatusUploading || task.StagedObjectID != 23 || task.LeaseVersion != 1 ||
		task.LeaseOwner != nil || task.LeaseUntil != nil || task.ReservedBytes != size || task.PartCount == nil || *task.PartCount != 2 {
		t.Fatal("首轮任务状态、租约、分片计划或预占不符")
	}
	var reserved, used uint64
	if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,used_storage FROM users WHERE id=7").Scan(&reserved, &used); err != nil ||
		reserved != size || used != 0 {
		t.Fatal("账号预占或已用空间不符", reserved, used, err)
	}
	parts, err := core.ListObjectParts(ctx, bucket.name, key, uploadID, 0, 1)
	if err != nil || parts.Key != key || parts.UploadID != uploadID || parts.ChecksumAlgorithm != "SHA256" ||
		parts.IsTruncated || len(parts.ObjectParts) != 0 {
		t.Fatal("数据库绑定的 MinIO 会话或分片事实不符", minio.ToErrorResponse(err).Code)
	}

	// 3. 先停 HTTP，放行首轮汇报后检查 worker 和连接的释放顺序。
	stop()
	select {
	case <-gate.httpStopped:
	case <-runDone:
		t.Fatal("HTTP 正常停止前服务已退出", runError)
	case <-ctx.Done():
		t.Fatal("HTTP 未在期限内停止")
	}
	select {
	case <-runDone:
		t.Fatal("初始化 worker 仍在汇报，服务已提前退出", runError)
	case <-time.After(100 * time.Millisecond):
	}
	releaseReport()
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
	for _, message := range []string{"HTTP 服务已停止", reportMessage, "分片上传初始化 worker 已停止", "MinIO 空闲连接已释放", "MySQL 连接池已关闭"} {
		position := strings.Index(logged, message)
		if position <= previous {
			t.Fatal("关停日志缺失或顺序不符", message, logged)
		}
		previous = position
	}
	if strings.Count(logged, reportMessage) != 1 ||
		!strings.Contains(logged, `"scanned":1,"bound":1,"retries_scheduled":0,"candidate_errors":0`) ||
		strings.Contains(logged, uploadID) {
		t.Fatal("首轮汇报计数或脱敏不符")
	}
	t.Logf("独占 MySQL %s，MinIO %s，HTTP %s。首轮自动绑定 1 条任务，关停先等 worker，再释放存储及数据库连接",
		mysqlContainer[:12], minioContainer[:12], address)
}
