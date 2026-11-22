package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"reflect"
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
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartUploadSessionCleanupLifecycleIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_SESSION_CLEANUP_LIFECYCLE_TEST") != "1" {
		t.Skip("显式启用后创建专用 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	project := "cloudnest-orphan-lifecycle-" + uuid.NewString()
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
	key, err := service.GenerateUploadObjectKey(7)
	if err != nil {
		t.Fatal(err)
	}

	// 1. 用固定 MinIO 的来源时间参数建立合成会话，保持主机与数据库时钟正常。
	core := minio.Core{Client: bucket.client}
	createSession := func(initiated time.Time) string {
		t.Helper()
		uploadID, err := core.NewMultipartUpload(ctx, bucket.name, key, minio.PutObjectOptions{
			Internal: minio.AdvancedPutOptions{SourceMTime: initiated},
		})
		if err != nil {
			t.Fatal("合成会话创建失败", err)
		}
		t.Cleanup(func() {
			cleanupContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := client.AbortMultipartUpload(cleanupContext, key, uploadID); err != nil && !errors.Is(err, domain.ErrUploadPartSessionMissing) {
				t.Error("专用合成会话清理失败", err)
			}
		})
		return uploadID
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	boundID := createSession(now.Add(-31 * time.Hour))
	orphanID := createSession(now.Add(-25 * time.Hour))
	recentID := createSession(now.Add(-23 * time.Hour))
	page, err := client.ListMultipartUploadSessionsPage(ctx, key, "", 3)
	if err != nil || page == nil || len(page.Sessions) != 3 {
		t.Fatal("未取得三条合成会话", err)
	}
	for index, hours := range []time.Duration{31, 25, 23} {
		if !page.Sessions[index].InitiatedAt.Equal(now.Add(-hours * time.Hour)) {
			t.Fatal("隔离存储没有保留合成发起时间", index, page.Sessions[index].InitiatedAt)
		}
	}

	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	const partSize, fileSize, initialUsed = 16 << 20, (16 << 20) + 1, 1024
	execute("INSERT INTO users(id,username,display_name,password_hash,used_storage,reserved_storage) VALUES(7,'orphan_lifecycle','合成账号','fixture_hash',?,?)", initialUsed, fileSize)
	execute("INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(23,7,?,?,?,?)",
		settings.Provider, bucket.name, key, fileSize)
	execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
		expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
		VALUES(19,7,11,23,13,'fixture.bin','MULTIPART',?,UNHEX(REPEAT('ab',32)),?,?,2,'UPLOADING',?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`,
		boundID, fileSize, partSize, fileSize)
	beforeTask, err := store.FindUserUploadTask(ctx, 7, 19)
	if err != nil {
		t.Fatal(err)
	}
	beforeObject, err := store.FindUserStoredObject(ctx, 7, 23)
	if err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name, proofProject string
	}{
		{"未选定实例证明项目", ""},
		{"受信实例启动并等待关停", project},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 2. 启动正式 run。启用时暂停首轮汇报，读取存储与数据库验证清理范围。
			address := reserveWebLoginAddress(t)
			t.Setenv("CLOUDNEST_HTTP_ADDR", address)
			t.Setenv("CLOUDNEST_DB_NAME", databaseName)
			t.Setenv("CLOUDNEST_TEMP_DIR", t.TempDir())
			t.Setenv("CLOUDNEST_JWT_SECRET", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("c"), 32)))
			t.Setenv("CLOUDNEST_MINIO_PROOF_COMPOSE_PROJECT", scenario.proofProject)
			const reportMessage = "分片会话收尾轮次结束"
			var logs bytes.Buffer
			gate := &serverWorkerReportGate{
				Handler: logging.New(&logs).Handler(), reportMessage: reportMessage, reportStarted: make(chan struct{}),
				releaseReport: make(chan struct{}), httpStopped: make(chan struct{}), deadline: ctx.Done(),
			}
			release := sync.OnceFunc(func() { close(gate.releaseReport) })
			serviceContext, stop := context.WithCancel(ctx)
			runDone := make(chan struct{})
			var runError error
			go func() {
				defer close(runDone)
				runError = run(serviceContext, slog.New(gate))
			}()
			defer func() {
				stop()
				release()
				select {
				case <-runDone:
				case <-time.After(35 * time.Second):
					t.Error("隔离服务未在清理期限内退出")
				}
			}()
			startupContext, stopStartup := context.WithTimeout(ctx, 25*time.Second)
			defer stopStartup()
			waitForServerIntegrationHTTP(t, startupContext, address)
			if scenario.proofProject != "" {
				select {
				case <-gate.reportStarted:
				case <-runDone:
					t.Fatal("首轮前隔离服务已退出", runError)
				case <-startupContext.Done():
					t.Fatal("首轮未按时执行")
				}
			}
			for _, uploadID := range []string{boundID, recentID} {
				if _, err := client.ListUploadPartsPage(ctx, key, uploadID, 0, 1); err != nil {
					t.Fatal("已绑定或宽限期内的会话应保持可读", err)
				}
			}
			_, err := client.ListUploadPartsPage(ctx, key, orphanID, 0, 1)
			if scenario.proofProject == "" && err != nil {
				t.Fatal("未选择实例证明时旧会话应保持可读", err)
			}
			if scenario.proofProject != "" && !errors.Is(err, domain.ErrUploadPartSessionMissing) {
				t.Fatal("首轮未移除超过 24 小时的未绑定合成会话", err)
			}
			afterTask, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil || !reflect.DeepEqual(beforeTask, afterTask) {
				t.Fatal("会话收尾改变了上传任务", err)
			}
			afterObject, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil || !reflect.DeepEqual(beforeObject, afterObject) {
				t.Fatal("会话收尾改变了原对象记录", err)
			}
			var used, reserved uint64
			if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&used, &reserved); err != nil ||
				used != initialUsed || reserved != fileSize {
				t.Fatal("会话收尾改变了配额", used, reserved, err)
			}

			// 3. 停止 HTTP 后保持汇报暂停，确认服务先等 worker，再释放连接。
			stop()
			if scenario.proofProject != "" {
				select {
				case <-gate.httpStopped:
				case <-ctx.Done():
					t.Fatal("隔离 HTTP 未及时停止")
				}
				select {
				case <-runDone:
					t.Fatal("worker 汇报前服务提前退出", runError)
				case <-time.After(100 * time.Millisecond):
				}
			}
			release()
			select {
			case <-runDone:
				if runError != nil {
					t.Fatal("隔离服务关停失败", runError)
				}
			case <-ctx.Done():
				t.Fatal("隔离服务关停超时")
			}
			logged := logs.String()
			const workerStarted = "分片会话收尾 worker 已启动"
			if scenario.proofProject == "" {
				if strings.Contains(logged, workerStarted) || strings.Contains(logged, reportMessage) {
					t.Fatal("未选定证明项目时仍启动了会话收尾")
				}
				return
			}
			previous := -1
			for _, message := range []string{"HTTP 服务已停止", reportMessage, "分片会话收尾 worker 已停止", "MinIO 空闲连接已释放", "MySQL 连接池已关闭"} {
				position := strings.Index(logged, message)
				if position <= previous {
					t.Fatal("服务关停次序不符", message)
				}
				previous = position
			}
			if !strings.Contains(logged, workerStarted) || strings.Count(logged, reportMessage) != 1 ||
				!strings.Contains(logged, `"interval_ms":60000`) || !strings.Contains(logged, `"scanned_objects":1,"scanned_pages":1,"scanned_sessions":3`) ||
				!strings.Contains(logged, `"processed":1,"skipped":0,"confirmed":1,"unconfirmed":0,"failed":0`) {
				t.Fatal("隔离首轮的汇报数、范围或确认结果不符")
			}
			t.Log("正式启动只收尾超 24 小时未绑定会话，保护会话保持可读，汇报后再释放 MinIO 和 MySQL")
		})
	}
}
