package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestServerOfflineSyncDeviceStartupIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_OFFLINE_SYNC_DEVICE_STARTUP_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_OFFLINE_SYNC_DEVICE_STARTUP_TEST=1 后使用独占 MySQL 和回环服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// 1. 在独占库准备 101 个离线候选，以及近期同步和已撤销的合成设备。
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备离线重建启动夹具失败", err)
		}
	}
	var databaseTime time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&databaseTime); err != nil {
		t.Fatal(err)
	}
	oldActivity := databaseTime.AddDate(0, 0, -91)
	recentActivity := databaseTime.AddDate(0, 0, -89)
	createdAt := databaseTime.AddDate(0, 0, -92)
	const originalGeneration = "A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1A1"
	execute(`INSERT INTO users(id,username,display_name,password_hash,change_seq)
		VALUES (7,'offline_startup','离线重建启动夹具','fixture_hash',7)`)
	execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform,revoked_at)
		WITH RECURSIVE fixture_ids AS (
			SELECT 1 AS number
			UNION ALL SELECT number+1 FROM fixture_ids WHERE number<103
		)
		SELECT number,7,CONCAT('28ee758a-0727-42de-ae7d-',LPAD(number,12,'0')),'Fixture','WEB',
			CASE WHEN number=103 THEN ? ELSE NULL END
		FROM fixture_ids`, databaseTime)
	execute(`INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at,created_at,updated_at)
		SELECT id,user_id,'ACTIVE',7,UNHEX(?),CASE WHEN id=102 THEN ? ELSE ? END,?,?
		FROM user_devices WHERE user_id=7`, originalGeneration, recentActivity, oldActivity, createdAt, createdAt)

	// 2. 从正式入口启动，以日志门闩暂停首轮汇报后独立读取已提交结果。
	t.Setenv("CLOUDNEST_STORAGE_PROVIDER", "local_minio")
	prepareServerReadOnlyStorageStub(t, "fixture-offline-sync-startup")
	address := configureBackupFixtureServer(t, databaseName)
	var logs bytes.Buffer
	const reportMessage = "离线设备重建轮次结束"
	gate := &serverWorkerReportGate{
		Handler: logging.New(&logs).Handler(), reportMessage: reportMessage,
		reportStarted: make(chan struct{}), releaseReport: make(chan struct{}),
		httpStopped: make(chan struct{}), deadline: ctx.Done(),
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
		t.Fatal("服务在首轮离线重建汇报前退出", runError)
	case <-ctx.Done():
		t.Fatal("离线重建首轮未在期限内汇报")
	}
	waitForServerIntegrationHTTP(t, ctx, address)

	rows, err := pool.QueryContext(ctx, `SELECT device_id,status,ack_sequence,HEX(cursor_generation),
		last_seen_at,created_at,updated_at FROM sync_devices WHERE user_id=7 ORDER BY device_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		var deviceID, ackSequence uint64
		var status domain.SyncDeviceStatus
		var generation string
		var lastSeen, created, updated time.Time
		if err := rows.Scan(&deviceID, &status, &ackSequence, &generation, &lastSeen, &created, &updated); err != nil {
			t.Fatal(err)
		}
		wantSeen := oldActivity
		if deviceID == 102 {
			wantSeen = recentActivity
		}
		if deviceID != uint64(count) || !lastSeen.Equal(wantSeen) || !created.Equal(createdAt) {
			t.Fatalf("设备 %d 的位置或时间不符：%d，%v，%v", deviceID, count, lastSeen, created)
		}
		if deviceID <= 100 {
			if status != domain.SyncDeviceStatusResetRequired || ackSequence != 0 ||
				generation == originalGeneration || generation == strings.Repeat("0", 32) || updated.Before(databaseTime) {
				t.Fatalf("设备 %d 未正确标记：状态 %s，位置 %d，时间 %v", deviceID, status, ackSequence, updated)
			}
		} else if status != domain.SyncDeviceStatusActive || ackSequence != 7 ||
			generation != originalGeneration || !updated.Equal(createdAt) {
			t.Fatalf("设备 %d 被首轮意外标记：状态 %s，位置 %d，时间 %v", deviceID, status, ackSequence, updated)
		}
	}
	if err := rows.Err(); err != nil || count != 103 {
		t.Fatalf("首轮设备总数不符：%d，错误 %v", count, err)
	}
	var accountSequence, events uint64
	if err := pool.QueryRowContext(ctx, `SELECT change_seq,(SELECT COUNT(*) FROM change_events WHERE user_id=7)
		FROM users WHERE id=7`).Scan(&accountSequence, &events); err != nil || accountSequence != 7 || events != 0 {
		t.Fatalf("首轮改变了事件或账号序号：事件 %d，序号 %d，错误 %v", events, accountSequence, err)
	}

	// 3. 发出停止信号，汇报完成后按顺序等待任务和连接退出。
	stop()
	select {
	case <-gate.httpStopped:
	case <-runDone:
		t.Fatal("服务在 HTTP 正常关闭前已经退出", runError)
	case <-ctx.Done():
		t.Fatal("隔离 HTTP 未在期限内关闭")
	}
	select {
	case <-runDone:
		t.Fatal("离线重建汇报仍在等待，服务已提前退出", runError)
	case <-time.After(100 * time.Millisecond):
	}
	releaseReport()
	select {
	case <-runDone:
		if runError != nil {
			t.Fatal("隔离服务关停失败", runError)
		}
	case <-ctx.Done():
		t.Fatal("放行汇报后服务未在期限内退出")
	}

	logText := logs.String()
	lastPosition := -1
	for _, message := range []string{"HTTP 服务已停止", reportMessage, "同步维护 worker 已停止",
		"MinIO 空闲连接已释放", "MySQL 连接池已关闭"} {
		position := strings.Index(logText, message)
		if position <= lastPosition {
			t.Fatalf("关停日志缺失或顺序不符：%s\n%s", message, logText)
		}
		lastPosition = position
	}
	if strings.Count(logText, reportMessage) != 1 || strings.Contains(logText, "同步日志裁剪轮次结束") ||
		!strings.Contains(logText, `"interval_ms":3600000,"offline_batch_limit":100,"offline_page_size":50`) ||
		!strings.Contains(logText, `"scanned":100,"marked":100,"stale_candidates":0,"candidate_errors":0,"next_after_device_id":100`) {
		t.Fatalf("首轮汇报或启动参数不符：%s", logText)
	}
	t.Logf("独占库 %s，回环服务 %s。首轮标记 100 台，保留超额、近期同步和已撤销各一台；完成汇报及 worker 退出后才释放存储和 MySQL",
		databaseName, address)
}
