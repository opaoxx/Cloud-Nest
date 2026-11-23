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

func TestServerSyncMaintenanceStartupIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_SYNC_MAINTENANCE_STARTUP_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_SYNC_MAINTENANCE_STARTUP_TEST=1 后使用独占 MySQL 和回环服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// 1. 在独占库准备一个进入待重建的设备和一个仍保护日志的活动设备。
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	var databaseTime time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&databaseTime); err != nil {
		t.Fatal(err)
	}
	execute(`INSERT INTO users(id,username,display_name,password_hash,change_seq) VALUES
		(7,'maintenance_startup_offline','离线账号','fixture_hash',20),
		(8,'maintenance_startup_active','近期同步账号','fixture_hash',19)`)
	execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES
		(17,7,'28ee758a-0727-42de-ae7d-3bb33155e157','Fixture','WEB'),
		(18,8,'28ee758a-0727-42de-ae7d-3bb33155e158','Fixture','WEB')`)
	execute(`INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at) VALUES
		(17,7,'ACTIVE',17,UNHEX(REPEAT('11',16)),?),
		(18,8,'ACTIVE',17,UNHEX(REPEAT('12',16)),?)`, databaseTime.AddDate(0, 0, -91), databaseTime.AddDate(0, 0, -89))
	const insertEvent = "INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload,created_at) " +
		"VALUES(?,?,11,1,'FIXTURE_CHANGE','{}',?)"
	for _, userID := range []uint64{7, 8} {
		for _, sequence := range []uint64{18, 19} {
			execute(insertEvent, userID, sequence, databaseTime.AddDate(0, 0, -92))
		}
	}
	execute(insertEvent, 7, 20, databaseTime.AddDate(0, 0, -89))

	// 2. 从正式启动入口运行。暂停裁剪汇报，在独立连接检查已提交结果。
	t.Setenv("CLOUDNEST_STORAGE_PROVIDER", "local_minio")
	prepareServerReadOnlyStorageStub(t, "fixture-sync-maintenance-startup")
	address := configureBackupFixtureServer(t, databaseName)
	var logs bytes.Buffer
	const pruneReport = "同步日志裁剪轮次结束"
	gate := &serverWorkerReportGate{
		Handler: logging.New(&logs).Handler(), reportMessage: pruneReport,
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
		t.Fatal("服务在首轮裁剪汇报前退出", runError)
	case <-ctx.Done():
		t.Fatal("首轮裁剪未在期限内汇报")
	}
	waitForServerIntegrationHTTP(t, ctx, address)

	for _, fixture := range []struct {
		userID, deviceID uint64
		status           domain.SyncDeviceStatus
		ack              uint64
		highWater        uint64
		count            uint64
		firstSequence    uint64
	}{
		{7, 17, domain.SyncDeviceStatusResetRequired, 0, 20, 1, 20},
		{8, 18, domain.SyncDeviceStatusActive, 17, 19, 2, 18},
	} {
		var status domain.SyncDeviceStatus
		var ack, highWater, count, firstSequence uint64
		err := pool.QueryRowContext(ctx, "SELECT states.status, states.ack_sequence, account.change_seq, "+
			"(SELECT COUNT(*) FROM change_events WHERE user_id=account.id), "+
			"(SELECT MIN(`sequence`) FROM change_events WHERE user_id=account.id) "+
			"FROM users AS account JOIN sync_devices AS states ON states.user_id=account.id "+
			"WHERE account.id=? AND states.device_id=?", fixture.userID, fixture.deviceID).
			Scan(&status, &ack, &highWater, &count, &firstSequence)
		if err != nil || status != fixture.status || ack != fixture.ack || highWater != fixture.highWater ||
			count != fixture.count || firstSequence != fixture.firstSequence {
			t.Fatalf("账号 %d 的首轮状态不符：%s，确认 %d，最大 %d，日志 %d 条，首条 %d，错误 %v",
				fixture.userID, status, ack, highWater, count, firstSequence, err)
		}
	}

	// 3. 关停 HTTP 后继续等待裁剪汇报。维护任务退出后才释放存储和 MySQL。
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
		t.Fatal("裁剪汇报仍在等待，服务已提前退出", runError)
	case <-time.After(100 * time.Millisecond):
	}
	releaseReport()
	select {
	case <-runDone:
		if runError != nil {
			t.Fatal("隔离服务关停失败", runError)
		}
	case <-ctx.Done():
		t.Fatal("放行裁剪汇报后服务未在期限内退出")
	}

	logText := logs.String()
	lastPosition := -1
	for _, message := range []string{"同步维护 worker 已启动", "离线设备重建轮次结束", "HTTP 服务已停止",
		pruneReport, "同步维护 worker 已停止", "MinIO 空闲连接已释放", "MySQL 连接池已关闭"} {
		position := strings.Index(logText, message)
		if position <= lastPosition {
			t.Fatalf("启动或关停日志顺序不符：%s\n%s", message, logText)
		}
		lastPosition = position
	}
	if strings.Count(logText, "同步维护 worker 已启动") != 1 || strings.Count(logText, "离线设备重建轮次结束") != 1 ||
		strings.Count(logText, pruneReport) != 1 || strings.Contains(logText, "离线设备重建 worker 已启动") ||
		!strings.Contains(logText, `"scanned":2,"pruned_accounts":1,"deleted_events":2,"empty_accounts":1,"account_errors":0,"next_after_user_id":0`) {
		t.Fatalf("首轮运行次数或裁剪汇报不符：%s", logText)
	}
	t.Logf("独占库 %s，回环服务 %s。首轮先重建后裁剪，保留近期日志与活动设备未确认日志；先结束裁剪汇报再释放存储和 MySQL",
		databaseName, address)
}
