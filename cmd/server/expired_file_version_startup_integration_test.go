package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerExpiredFileVersionStartupIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_EXPIRED_FILE_VERSION_STARTUP_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_EXPIRED_FILE_VERSION_STARTUP_TEST=1 后启动独占 MySQL 和回环服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// 1. 在独占库中准备 101 条到期历史和一条保留期内历史。
	mysqlContainer := startIsolatedWebMySQL(t, ctx)
	t.Logf("历史到期启动专用 MySQL %s", mysqlContainer[:12])
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	execute := func(statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备历史首轮夹具失败", err)
		}
	}
	execute(`INSERT INTO users(id,username,display_name,password_hash,used_storage,reserved_storage)
		VALUES(7,'history_startup','历史到期启动夹具','fixture_hash',723,19)`)
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,verified_sha256,status,verified_at)
		VALUES(29,7,'local_minio','fixture-history-startup','current',7,UNHEX(REPEAT('11',32)),'READY',UTC_TIMESTAMP(6)),
		      (30,7,'local_minio','fixture-history-startup','last-version',9,UNHEX(REPEAT('22',32)),'READY',UTC_TIMESTAMP(6))`)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,status,revision,object_id,file_size,purged_at)
		VALUES(31,7,13,'current.bin',X'63','FILE','OTHER','ACTIVE',103,29,7,NULL),
		      (32,7,13,'purged.bin',X'70','FILE','OTHER','PURGED',2,NULL,9,UTC_TIMESTAMP(6))`)
	execute(`INSERT INTO file_versions(id,user_id,entry_id,revision,object_id,file_size,created_at,retained_until)
		WITH RECURSIVE fixture_ids AS (
			SELECT 1 AS number
			UNION ALL SELECT number+1 FROM fixture_ids WHERE number<100
		)
		SELECT 1000+number,7,31,number,29,7,UTC_TIMESTAMP(6)-INTERVAL 31 DAY,UTC_TIMESTAMP(6)-INTERVAL 1 DAY
		FROM fixture_ids`)
	execute(`INSERT INTO file_versions(id,user_id,entry_id,revision,object_id,file_size,created_at,retained_until)
		VALUES(900,7,32,1,30,9,UTC_TIMESTAMP(6)-INTERVAL 32 DAY,UTC_TIMESTAMP(6)-INTERVAL 2 DAY),
		      (1200,7,31,101,29,7,UTC_TIMESTAMP(6)-INTERVAL 29 DAY,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`)

	// 2. 从正式 run 启动隔离 HTTP，暂停首轮汇报以读回已提交结果。
	t.Setenv("CLOUDNEST_STORAGE_PROVIDER", "local_minio")
	prepareServerReadOnlyStorageStub(t, "fixture-history-startup")
	address := configureBackupFixtureServer(t, databaseName)
	var logs bytes.Buffer
	const reportMessage = "历史到期结算轮次结束"
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
		t.Fatal("服务在首轮历史结算汇报前退出", runError)
	case <-ctx.Done():
		t.Fatal("历史首轮结算未在期限内汇报")
	}
	waitForServerIntegrationHTTP(t, ctx, address)

	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.ListExpiredFileVersionCandidates(ctx, 100)
	if err != nil || len(next) != 1 || next[0].ID != 1100 {
		t.Fatal("首次结算后应只剩批量之外的一条到期项", next, err)
	}
	rows, err := pool.QueryContext(ctx, "SELECT id FROM file_versions WHERE user_id=7 ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var remaining []uint64
	for rows.Next() {
		var versionID uint64
		if err := rows.Scan(&versionID); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		remaining = append(remaining, versionID)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil || !slices.Equal(remaining, []uint64{1100, 1200}) {
		t.Fatal("超出批量上限或未到期的历史未保留", remaining, err)
	}
	var used, reserved, sequence uint64
	if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage,change_seq FROM users WHERE id=7").
		Scan(&used, &reserved, &sequence); err != nil || used != 21 || reserved != 19 || sequence != 0 {
		t.Fatal("首轮账目或变更位置不符", used, reserved, sequence, err)
	}
	shared, err := store.FindUserStoredObject(ctx, 7, 29)
	if err != nil || shared.Status != domain.StoredObjectStatusReady || shared.GCAfter != nil {
		t.Fatal("仍有引用的对象不应被登记回收", err)
	}
	unreferenced, err := store.FindUserStoredObject(ctx, 7, 30)
	if err != nil || unreferenced.Status != domain.StoredObjectStatusReady || unreferenced.UnreferencedAt == nil ||
		unreferenced.GCAfter == nil || !unreferenced.GCAfter.Equal(unreferenced.UnreferencedAt.Add(7*24*time.Hour)) {
		t.Fatal("最后引用解除后未登记七天宽限期", err)
	}

	// 3. 在汇报暂停时关闭服务，放行后验证 worker 先于连接退出。
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
		t.Fatal("历史轮次仍在汇报，服务已提前退出", runError)
	case <-time.After(100 * time.Millisecond):
	}
	releaseReport()
	select {
	case <-runDone:
		if runError != nil {
			t.Fatal("隔离服务关停失败", runError)
		}
	case <-ctx.Done():
		t.Fatal("汇报完成后服务仍未退出")
	}
	logText := logs.String()
	lastPosition := -1
	for _, message := range []string{"HTTP 服务已停止", reportMessage, "历史到期结算 worker 已停止",
		"MinIO 空闲连接已释放", "MySQL 连接池已关闭"} {
		position := strings.Index(logText, message)
		if position <= lastPosition {
			t.Fatal("关停日志缺失或顺序不符", message, logText)
		}
		lastPosition = position
	}
	if strings.Count(logText, reportMessage) != 1 || !strings.Contains(logText, `"interval_ms":3600000,"batch_limit":100`) ||
		!strings.Contains(logText, `"scanned":100,"settled":100,"stale_candidates":0,"candidate_errors":0`) {
		t.Fatal("首轮汇报或启动参数不符", logText)
	}
	t.Logf("首轮扫描并结算 100 条，保留历史 %v，已用 723→%d B，预占 %d B，最后引用对象登记七天；依序完成汇报、停止 worker、释放存储和 MySQL 连接",
		remaining, used, reserved)
}
