package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerSyncMaintenanceRoundIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_SYNC_MAINTENANCE_ORDER_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_SYNC_MAINTENANCE_ORDER_TEST=1 后使用独占 MySQL")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// 1. 用正式迁移准备离线和近期活动设备。两台设备均未确认任何保留日志。
	pool, _ := prepareServerIntegrationDatabase(t, ctx)
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
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
		(7,'maintenance_offline','离线账号','fixture_hash',19),
		(8,'maintenance_recent','近期同步账号','fixture_hash',19)`)
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
	observer, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	assertDevice := func(deviceID uint64, want domain.SyncDeviceStatus, wantAck uint64) {
		t.Helper()
		var status domain.SyncDeviceStatus
		var ack uint64
		err := observer.QueryRowContext(ctx, "SELECT status,ack_sequence FROM sync_devices WHERE device_id=?", deviceID).Scan(&status, &ack)
		if err != nil || status != want || ack != wantAck {
			t.Fatalf("设备 %d 已提交状态不符：%s，位置 %d，错误 %v", deviceID, status, ack, err)
		}
	}
	assertDevice(17, domain.SyncDeviceStatusActive, 17)

	// 2. 让真实离线批次先提交，裁剪阶段启动前从独立连接确认两台设备状态。
	offlineBatch := service.NewOfflineSyncDeviceBatch(store.ReadOfflineSyncDeviceCutoff, store.ListOfflineSyncDeviceCandidates, store.MarkOfflineSyncDeviceResetRequired)
	pruneBatch := service.NewSyncChangePruneBatch(store.ReadSyncChangePruneCutoff, store.ListSyncChangePruneCandidates, store.PruneSyncChangeLogBatch)
	result, err := runServerSyncMaintenanceRound(ctx, 0, 0,
		func(roundContext context.Context, after uint64) (service.OfflineSyncDeviceBatchResult, error) {
			return offlineBatch.RunOnce(roundContext, after, 100, 50)
		},
		func(roundContext context.Context, after uint64) (service.SyncChangePruneBatchResult, error) {
			assertDevice(17, domain.SyncDeviceStatusResetRequired, 0)
			assertDevice(18, domain.SyncDeviceStatusActive, 17)
			return pruneBatch.RunOnce(roundContext, after, service.SyncChangePruneBatchLimits{ScanLimit: 100, PageSize: 50, AccountDeleteLimit: 100})
		},
	)
	if err != nil || !result.PruneStarted || result.Offline.Marked != 1 || len(result.Offline.CandidateErrors) != 0 ||
		result.Prune.PrunedAccounts != 1 || result.Prune.DeletedEvents != 2 || result.Prune.EmptyAccounts != 1 || len(result.Prune.AccountErrors) != 0 {
		t.Fatalf("有序同步维护轮次未按期望完成：%+v，错误 %v", result, err)
	}

	// 3. 被重建设备的旧日志已裁剪。近期设备保护的旧日志和账号累计序号继续保留。
	for userID, wantCount := range map[uint64]uint64{7: 0, 8: 2} {
		var highWater, count uint64
		err := observer.QueryRowContext(ctx, `SELECT change_seq, (SELECT COUNT(*) FROM change_events WHERE user_id=users.id)
			FROM users WHERE id=?`, userID).Scan(&highWater, &count)
		if err != nil || highWater != 19 || count != wantCount {
			t.Fatalf("账号 %d 的已提交日志不符：最大序号 %d，条数 %d，错误 %v", userID, highWater, count, err)
		}
	}
	t.Log("独立连接确认离线状态已提交后才进入裁剪，近期同步设备仍保护日志")
}
