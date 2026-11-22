package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerExpiredTrashRootIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_SERVER_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_SERVER_TEST=1 后用独占库验证到期标记启动")
	}
	// 1. 创建专用容器和独占库，准备到期与保留期内的根项。
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	mysqlContainer := startIsolatedWebMySQL(t, ctx)
	t.Logf("到期标记专用 MySQL %s", mysqlContainer[:12])
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	execute := func(statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备到期标记启动夹具失败", err)
		}
	}
	execute("INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_expiry','到期测试用户','fixture_hash')")
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(11,1,'',X'','DIR')")
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,revision)
		WITH RECURSIVE fixture_ids AS (
			SELECT 1 AS number
			UNION ALL SELECT number+1 FROM fixture_ids WHERE number<101
		)
		SELECT 1000+number,1,11,CONCAT('fixture-',number),CAST(CONCAT('fixture-',number) AS BINARY),
		       'DIR','TRASHED',UTC_TIMESTAMP(6)-INTERVAL 31 DAY,3
		FROM fixture_ids`)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,revision)
		VALUES(1200,1,11,'fixture-retained',CAST('fixture-retained' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6)-INTERVAL 29 DAY,3)`)

	// 2. 启动隔离服务，存储桩只接受桶检查，永久清理使用测试空实现。
	prepareServerReadOnlyStorageStub(t, "fixture-expiry-startup")
	t.Setenv("CLOUDNEST_DB_NAME", databaseName)
	t.Setenv("CLOUDNEST_TEMP_DIR", t.TempDir())
	t.Setenv("CLOUDNEST_JWT_SECRET", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("e"), 32)))
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
	const reportMessage = "回收站到期标记轮次结束"
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
		runError = runWithPurgeRootWorker(runContext, slog.New(gate),
			func(context.Context, func(context.Context, int, int) (service.PurgeRootBatchResult, error), *slog.Logger) func() {
				return func() {}
			})
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
		t.Fatal("服务在首轮到期标记汇报前退出", runError, logs.String())
	case <-ctx.Done():
		t.Fatal("到期标记首轮未在期限内汇报")
	}
	waitForServerIntegrationHTTP(t, ctx, address)

	// 3. 首轮最多标记最早的 100 项，剩余到期项和未到期项继续保留。
	var count, first, last uint64
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*),MIN(id),MAX(id) FROM file_entries
		WHERE user_id=1 AND status='PURGING' AND revision=4 AND purge_started_at IS NOT NULL AND purged_at IS NULL`).
		Scan(&count, &first, &last); err != nil || count != 100 || first != 1001 || last != 1100 {
		t.Fatal("首轮标记数量、范围或持久状态不符", count, first, last, err)
	}
	var remaining uint64
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM file_entries
		WHERE user_id=1 AND id IN (1101,1200) AND status='TRASHED' AND revision=3 AND purge_started_at IS NULL`).
		Scan(&remaining); err != nil || remaining != 2 {
		t.Fatal("超限项或保留期内条目被提前标记", remaining, err)
	}
	var changeSequence, events, matchedEvents uint64
	if err := pool.QueryRowContext(ctx, `SELECT change_seq,
		(SELECT COUNT(*) FROM change_events WHERE user_id=users.id),
		(SELECT COUNT(*) FROM change_events AS events
		 JOIN file_entries AS entries ON entries.user_id=events.user_id AND entries.id=events.entry_id
		 WHERE events.user_id=users.id AND entries.status='PURGING'
		   AND events.event_type='ENTRY_PURGE_STARTED' AND events.sequence=entries.id-1000 AND events.revision=entries.revision
		   AND JSON_UNQUOTE(JSON_EXTRACT(events.payload,'$.entry_id'))=CAST(entries.id AS CHAR)
		   AND JSON_UNQUOTE(JSON_EXTRACT(events.payload,'$.parent_id'))=CAST(entries.parent_id AS CHAR)
		   AND JSON_UNQUOTE(JSON_EXTRACT(events.payload,'$.name'))=entries.name
		   AND JSON_UNQUOTE(JSON_EXTRACT(events.payload,'$.revision'))=CAST(entries.revision AS CHAR))
		FROM users WHERE id=1`).Scan(&changeSequence, &events, &matchedEvents); err != nil ||
		changeSequence != 100 || events != 100 || matchedEvents != 100 {
		t.Fatal("到期标记和事件未逐项提交，或账号序号不连续", changeSequence, events, matchedEvents, err)
	}

	// 4. 在汇报暂停时停止 HTTP。放行后验证最后汇报、worker 退出和数据库释放的顺序。
	stop()
	select {
	case <-gate.httpStopped:
	case <-runDone:
		t.Fatal("服务在 HTTP 正常停止前退出", runError)
	case <-ctx.Done():
		t.Fatal("HTTP 未在期限内关闭")
	}
	select {
	case <-runDone:
		t.Fatal("到期标记仍在汇报时服务提前退出", runError)
	case <-time.After(100 * time.Millisecond):
	}
	releaseReport()
	select {
	case <-runDone:
		if runError != nil {
			t.Fatal("隔离服务关停失败", runError)
		}
	case <-ctx.Done():
		t.Fatal("放行汇报后服务仍未退出")
	}
	logText := logs.String()
	lastPosition := -1
	for _, message := range []string{"HTTP 服务已停止", reportMessage, "回收站到期标记 worker 已停止", "MySQL 连接池已关闭"} {
		position := strings.Index(logText, message)
		if position <= lastPosition {
			t.Fatal("关停日志缺失或顺序不符", message, logText)
		}
		lastPosition = position
	}
	if strings.Count(logText, reportMessage) != 1 || strings.Contains(logText, "永久清理 worker 已启动") ||
		!strings.Contains(logText, `"interval_ms":3600000,"batch_limit":100`) ||
		!strings.Contains(logText, `"scanned":100,"marked":100,"stale_candidates":0,"candidate_errors":0`) {
		t.Fatal("启动参数、隔离范围或首轮汇报不符", logText)
	}
	t.Log("首轮标记 100 项，事件序号 1 至 100 与根项逐条一致，超限和保留期内各 1 项未标记。放行汇报后先停 worker，再关闭数据库池")
}

// prepareServerReadOnlyStorageStub 只为隔离进程提供本地桶检查接口。
func prepareServerReadOnlyStorageStub(t *testing.T, bucketName string) {
	t.Helper()
	storageStub := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/"+bucketName+"/" || request.URL.Path == "/"+bucketName {
			switch {
			case request.Method == http.MethodHead && request.URL.RawQuery == "":
				response.WriteHeader(http.StatusOK)
				return
			case request.Method == http.MethodGet && request.URL.Query().Has("location"):
				response.Header().Set("Content-Type", "application/xml")
				_, _ = io.WriteString(response, "<LocationConstraint>us-east-1</LocationConstraint>")
				return
			}
		}
		t.Error("隔离存储桩收到桶检查以外的请求", request.Method, request.URL.Path)
		http.Error(response, "unexpected request", http.StatusBadRequest)
	}))
	t.Cleanup(storageStub.Close)
	t.Setenv("CLOUDNEST_S3_ENDPOINT", storageStub.URL)
	t.Setenv("CLOUDNEST_S3_PUBLIC_ENDPOINT", storageStub.URL)
	t.Setenv("CLOUDNEST_S3_BUCKET", bucketName)
	t.Setenv("CLOUDNEST_S3_ACCESS_KEY", "fixture-key")
	t.Setenv("CLOUDNEST_S3_SECRET_KEY", "fixture-secret")
}
