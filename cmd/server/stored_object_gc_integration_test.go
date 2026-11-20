package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestServerStoredObjectGCIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_SERVER_TEST") != "1" || os.Getenv("CLOUDNEST_UPLOAD_STORAGE_TEST") != "1" {
		t.Skip("同时启用 CLOUDNEST_SERVER_TEST 和 CLOUDNEST_UPLOAD_STORAGE_TEST 后使用独占库和桶联测")
	}
	// 1. 准备独占库、启用版本控制的独占桶和三条对象记录。
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	target := prepareServerUploadContentBucket(t, ctx)
	t.Setenv("CLOUDNEST_DB_NAME", databaseName)
	t.Setenv("CLOUDNEST_TEMP_DIR", t.TempDir())
	t.Setenv("CLOUDNEST_JWT_SECRET", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("g"), 32)))
	if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_gc','回收测试用户','fixture_hash')"); err != nil {
		t.Fatal(err)
	}
	fixtures := []struct {
		id               uint64
		key              string
		initialStatus    domain.StoredObjectStatus
		daysUnreferenced int
		version          string
	}{
		{id: 1, key: "users/1/objects/gc-recover", initialStatus: domain.StoredObjectStatusDeleting, daysUnreferenced: 9},
		{id: 2, key: "users/1/objects/gc-due", initialStatus: domain.StoredObjectStatusOrphaned, daysUnreferenced: 8},
		{id: 3, key: "users/1/objects/gc-future", initialStatus: domain.StoredObjectStatusOrphaned, daysUnreferenced: 6},
	}
	for index := range fixtures {
		fixture := &fixtures[index]
		content := []byte(fixture.key)
		uploaded, err := target.client.PutObject(ctx, target.name, fixture.key, bytes.NewReader(content), int64(len(content)), minio.PutObjectOptions{})
		if err != nil || uploaded.VersionID == "" || uploaded.VersionID == "null" {
			t.Fatal("未在独占桶建立指定版本", fixture.key, err)
		}
		fixture.version = uploaded.VersionID
		_, err = pool.ExecContext(ctx, `INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,storage_version_id,
			file_size,status,unreferenced_at,gc_after)
			VALUES(?,1,'local_minio',?,?,?,?,?,UTC_TIMESTAMP(6)-INTERVAL ? DAY,UTC_TIMESTAMP(6)-INTERVAL ? DAY)`,
			fixture.id, target.name, fixture.key, fixture.version, len(content), fixture.initialStatus,
			fixture.daysUnreferenced, fixture.daysUnreferenced-7)
		if err != nil {
			t.Fatal(err)
		}
	}

	// 2. 启动真实 run，在首轮准备汇报时核对 MySQL 状态及 MinIO 版本。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDNEST_HTTP_ADDR", address)
	var logs bytes.Buffer
	gate := &storedObjectGCLogGate{
		Handler: logging.New(&logs).Handler(), reportStarted: make(chan struct{}),
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
			t.Error("联测服务未在清理期限内关闭")
		}
	}()
	select {
	case <-gate.reportStarted:
	case <-runDone:
		t.Fatal("服务在回收首轮汇报前退出", runError, logs.String())
	case <-ctx.Done():
		t.Fatal("回收首轮没有在期限内汇报")
	}
	waitForStoredObjectGCTestHTTP(t, ctx, address)
	for _, fixture := range fixtures {
		var status domain.StoredObjectStatus
		if err := pool.QueryRowContext(ctx, "SELECT status FROM stored_objects WHERE owner_user_id=1 AND id=?", fixture.id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		_, statError := target.client.StatObject(ctx, target.name, fixture.key, minio.StatObjectOptions{VersionID: fixture.version})
		if fixture.id == 3 {
			if status != fixture.initialStatus || statError != nil {
				t.Fatal("尚未到期的对象应保留原状态和版本", status, statError)
			}
		} else if status != domain.StoredObjectStatusDeleted || minio.ToErrorResponse(statError).StatusCode != http.StatusNotFound {
			t.Fatal("恢复和到期对象应写回 DELETED 并删掉对应版本", fixture.id, status, statError)
		}
	}
	t.Log("启动首轮恢复 1 项并回收新到期 1 项，指定版本均已删除，未到期对象仍存在")

	// 3. 让 HTTP 先关闭，轮次汇报仍暂停。放行后核对 worker、MinIO 和 MySQL 退出顺序。
	stop()
	select {
	case <-gate.httpStopped:
	case <-runDone:
		t.Fatal("服务提前退出", runError)
	case <-ctx.Done():
		t.Fatal("HTTP 没有在期限内停止")
	}
	select {
	case <-runDone:
		t.Fatal("回收 worker 尚未结束汇报，run 已返回", runError)
	case <-time.After(100 * time.Millisecond):
	}
	releaseReport()
	select {
	case <-runDone:
		if runError != nil {
			t.Fatal("服务关停失败", runError)
		}
	case <-ctx.Done():
		t.Fatal("释放汇报后服务仍未退出")
	}
	logText := logs.String()
	previous := -1
	for _, message := range []string{"HTTP 服务已停止", "对象回收轮次结束", "对象回收 worker 已停止", "MinIO 空闲连接已释放", "MySQL 连接池已关闭"} {
		position := strings.Index(logText, message)
		if position <= previous {
			t.Fatal("退出日志缺失或顺序不符", message, logText)
		}
		previous = position
	}
	if !strings.Contains(logText, `"recovery":{"scanned":1,"completed":1,"candidate_errors":0}`) ||
		!strings.Contains(logText, `"due":{"scanned":1,"completed":1,"stale_candidates":0,"candidate_errors":0}`) {
		t.Fatal("首轮的恢复与到期汇报不符", logText)
	}
	t.Log("HTTP 先停止，回收汇报结束和 worker 停止后才释放 MinIO、关闭 MySQL")
}

// storedObjectGCLogGate 只暂停回收汇报，允许其余日志并行写入。
type storedObjectGCLogGate struct {
	slog.Handler
	reportStarted, releaseReport, httpStopped chan struct{}
	deadline                                  <-chan struct{}
}

func (gate *storedObjectGCLogGate) Handle(ctx context.Context, record slog.Record) error {
	switch record.Message {
	case "对象回收轮次结束":
		close(gate.reportStarted)
		select {
		case <-gate.releaseReport:
		case <-gate.deadline:
		}
	case "HTTP 服务已停止":
		defer close(gate.httpStopped)
	}
	return gate.Handler.Handle(ctx, record)
}

func waitForStoredObjectGCTestHTTP(t *testing.T, ctx context.Context, address string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/health/live", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("联测 HTTP 服务未在期限内就绪")
		case <-ticker.C:
		}
	}
}
