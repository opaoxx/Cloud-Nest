package repository

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestOfflineSyncDeviceBatchIntegration(t *testing.T) {
	// 1. 准备两个账号的五台设备，固定数据库截止时间。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, _ := trashEntryTestDatabase(t, ctx)
	execute := func(statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到同步设备表定义")
	}
	execute(deviceDDL)
	reference := time.Date(2026, 11, 23, 12, 0, 0, 15625000, time.UTC)
	cutoff := reference.AddDate(0, 0, -90)
	offline := cutoff.Add(-time.Microsecond)
	generation := domain.SyncCursorGeneration{1, 0, 3, 0, 5, 0, 7, 0, 9, 0, 11, 0, 13, 0, 15, 0xfe}
	execute("UPDATE users SET change_seq=7 WHERE id IN (1,2)")
	for _, fixture := range []struct {
		deviceID, userID uint64
		lastSeen         time.Time
	}{{19, 1, offline}, {20, 1, offline}, {21, 1, cutoff}, {22, 2, offline}, {23, 2, offline}} {
		if fixture.deviceID != 19 {
			execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES (?, ?, ?, 'Fixture', 'WEB')`,
				fixture.deviceID, fixture.userID, fmt.Sprintf("28ee758a-0727-42de-ae7d-%012x", fixture.deviceID))
		}
		execute(`INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at)
			VALUES (?,?,'ACTIVE',7,?,?)`, fixture.deviceID, fixture.userID, generation[:], fixture.lastSeen)
	}
	execute("SET timestamp = ?", float64(reference.Unix())+0.015625)
	defer execute("SET timestamp = DEFAULT")

	// 2. 用真实仓储运行两轮。首页后推进数据库时钟，并模拟一条刷新、一条失败。
	type pageCall struct {
		after  uint64
		limit  int
		cutoff time.Time
	}
	var pages []pageCall
	var attemptedIDs []uint64
	reads := 0
	failure := errors.New("设备 19 的合成事务故障")
	batch := service.NewOfflineSyncDeviceBatch(func(queryContext context.Context) (time.Time, error) {
		reads++
		return store.ReadOfflineSyncDeviceCutoff(queryContext)
	}, func(queryContext context.Context, cutoffTime time.Time, after uint64, limit int) ([]domain.OfflineSyncDeviceCandidate, error) {
		pages = append(pages, pageCall{after: after, limit: limit, cutoff: cutoffTime})
		candidates, err := store.ListOfflineSyncDeviceCandidates(queryContext, cutoffTime, after, limit)
		if err == nil && len(pages) == 1 {
			execute("SET timestamp = ?", float64(reference.Unix())+1.015625)
			execute("UPDATE sync_devices SET last_seen_at=? WHERE device_id=20", reference)
		}
		return candidates, err
	}, func(markContext context.Context, cutoffTime time.Time, candidate domain.OfflineSyncDeviceCandidate) error {
		attemptedIDs = append(attemptedIDs, candidate.DeviceID)
		expectedCutoff := cutoff
		if reads == 2 {
			expectedCutoff = cutoff.Add(time.Second)
		}
		if cutoffTime != expectedCutoff {
			t.Fatalf("标记收到错误截止点：%v，期望 %v", cutoffTime, expectedCutoff)
		}
		if candidate.DeviceID == 19 {
			return failure
		}
		return store.MarkOfflineSyncDeviceResetRequired(markContext, cutoffTime, candidate)
	})
	first, err := batch.RunOnce(ctx, 0, 3, 2)
	if err != nil || first.Cutoff != cutoff || first.Scanned != 3 || first.Marked != 1 ||
		first.StaleCandidates != 1 || first.NextAfterDeviceID != 22 || reads != 1 ||
		!slices.Equal(pages, []pageCall{{0, 2, cutoff}, {20, 1, cutoff}}) || !slices.Equal(attemptedIDs, []uint64{19, 20, 22}) {
		t.Fatalf("首轮未按固定截止点分页和限量：%+v，错误 %v，扫描 %+v，尝试 %v", first, err, pages, attemptedIDs)
	}
	if len(first.CandidateErrors) != 1 || first.CandidateErrors[0].UserID != 1 || first.CandidateErrors[0].DeviceID != 19 ||
		!errors.Is(first.CandidateErrors[0].Err, failure) {
		t.Fatalf("首项失败未保留设备和原因：%+v", first.CandidateErrors)
	}

	// 3. 读回已提交结果，然后从首轮位置继续直到扫描结束。
	assertState := func(deviceID uint64, wantStatus domain.SyncDeviceStatus, wantSeen time.Time) {
		t.Helper()
		var status domain.SyncDeviceStatus
		var ack uint64
		var seen time.Time
		err := pool.QueryRowContext(ctx, "SELECT status,ack_sequence,last_seen_at FROM sync_devices WHERE device_id=?", deviceID).
			Scan(&status, &ack, &seen)
		wantAck := uint64(7)
		if wantStatus == domain.SyncDeviceStatusResetRequired {
			wantAck = 0
		}
		if err != nil || status != wantStatus || ack != wantAck || !seen.Equal(wantSeen) {
			t.Fatalf("设备 %d 状态不符：状态 %s、位置 %d、同步时间 %v、错误 %v", deviceID, status, ack, seen, err)
		}
	}
	assertState(19, domain.SyncDeviceStatusActive, offline)
	assertState(20, domain.SyncDeviceStatusActive, reference)
	assertState(21, domain.SyncDeviceStatusActive, cutoff)
	assertState(22, domain.SyncDeviceStatusResetRequired, offline)
	assertState(23, domain.SyncDeviceStatusActive, offline)

	second, err := batch.RunOnce(ctx, first.NextAfterDeviceID, 3, 2)
	laterCutoff := cutoff.Add(time.Second)
	if err != nil || second.Cutoff != laterCutoff || second.Scanned != 1 || second.Marked != 1 ||
		second.StaleCandidates != 0 || len(second.CandidateErrors) != 0 || second.NextAfterDeviceID != 0 ||
		reads != 2 || !slices.Equal(pages, []pageCall{{0, 2, cutoff}, {20, 1, cutoff}, {22, 2, laterCutoff}}) ||
		!slices.Equal(attemptedIDs, []uint64{19, 20, 22, 23}) {
		t.Fatalf("续扫结果不符：%+v，错误 %v，扫描 %+v，尝试 %v", second, err, pages, attemptedIDs)
	}
	assertState(23, domain.SyncDeviceStatusResetRequired, offline)
	t.Log("离线单轮上限 3 分两页，依次得到失败、失效、标记；时钟前进未改变首轮截止点，下一轮续扫后归零")
}
