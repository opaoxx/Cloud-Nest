package repository

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func prepareRestoredSyncDevicesIntegration(t *testing.T, ctx context.Context) (
	*Store, func(string, ...any), func() []domain.RestoredSyncDeviceCandidate,
) {
	t.Helper()
	pool, store, _ := trashEntryTestDatabase(t, ctx)
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到正式同步设备表")
	}
	execute(deviceDDL)
	execute("UPDATE users SET change_seq=7 WHERE id IN (1,2)")
	execute("UPDATE users SET status='DISABLED' WHERE id=2")
	execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform,revoked_at) VALUES
		(20,1,'28ee758a-0727-42de-ae7d-3bb33155e150','待重建','WEB',NULL),
		(21,1,'28ee758a-0727-42de-ae7d-3bb33155e151','故障项','WEB',NULL),
		(29,2,'28ee758a-0727-42de-ae7d-3bb33155e159','已撤销','WEB',UTC_TIMESTAMP(6))`)
	generation := domain.SyncCursorGeneration{0xa1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe}
	execute(`INSERT INTO sync_devices(device_id,user_id,status,cursor_generation,ack_sequence,last_seen_at) VALUES
		(19,1,'ACTIVE',?,7,UTC_TIMESTAMP(6)),(20,1,'RESET_REQUIRED',?,0,UTC_TIMESTAMP(6)),
		(21,1,'ACTIVE',?,5,UTC_TIMESTAMP(6)),(29,2,'ACTIVE',?,7,UTC_TIMESTAMP(6))`,
		generation[:], generation[:], generation[:], generation[:])
	readStates := func() []domain.RestoredSyncDeviceCandidate {
		t.Helper()
		devices, err := store.ListRestoredSyncDeviceCandidates(ctx, 0, 5)
		if err != nil || len(devices) != 4 {
			t.Fatalf("恢复同步设备范围不符：%+v，%v", devices, err)
		}
		return devices
	}
	execute(`CREATE TRIGGER fixture_restore_sync_batch_failure BEFORE UPDATE ON sync_devices FOR EACH ROW
		BEGIN IF OLD.device_id=21 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture reset failure'; END IF; END`)
	return store, execute, readStates
}

func TestRestoredSyncDeviceBatchIntegration(t *testing.T) {
	// 1. 准备两账号四条候选，在独占库的第二页注入可恢复的写入失败。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store, execute, readStates := prepareRestoredSyncDevicesIntegration(t, ctx)
	before := readStates()

	// 2. 调用实际仓储，核对每页参数和故障前已提交的设备。
	type pageCall struct {
		after uint64
		limit int
	}
	var pages []pageCall
	var attempts []uint64
	batch := service.NewRestoredSyncDeviceBatch(
		func(queryContext context.Context, after uint64, limit int) ([]domain.RestoredSyncDeviceCandidate, error) {
			pages = append(pages, pageCall{after, limit})
			return store.ListRestoredSyncDeviceCandidates(queryContext, after, limit)
		},
		func(txContext context.Context, candidate domain.RestoredSyncDeviceCandidate) error {
			attempts = append(attempts, candidate.DeviceID)
			return store.ResetRestoredSyncDevice(txContext, candidate)
		},
	)
	first, err := batch.RunOnce(ctx, 0, 3, 2)
	wantFirst := service.RestoredSyncDeviceBatchResult{Scanned: 3, Reset: 2, NextAfterDeviceID: 20, FailedUserID: 1, FailedDeviceID: 21}
	var databaseError *mysql.MySQLError
	if first != wantFirst || !errors.As(err, &databaseError) || databaseError.Number != 1644 ||
		!slices.Equal(pages, []pageCall{{0, 2}, {20, 1}}) || !slices.Equal(attempts, []uint64{19, 20, 21}) {
		t.Fatalf("首批结果或失败位置不符：%+v，%v，页 %v，尝试 %v", first, err, pages, attempts)
	}
	partial := readStates()
	if verified, err := service.VerifyRestoredSyncDeviceReset(ctx, before, 2, store.ListRestoredSyncDeviceCandidates); verified != 2 ||
		!errors.Is(err, service.ErrRestoredSyncDeviceResetIncomplete) || !slices.Equal(readStates(), partial) {
		t.Fatalf("部分换代目标应被只读复核拒绝：%d，%v", verified, err)
	}
	assertRotated := func(previous, current domain.RestoredSyncDeviceCandidate) {
		t.Helper()
		if current.UserID != previous.UserID || current.DeviceID != previous.DeviceID ||
			current.State.Status != domain.SyncDeviceStatusResetRequired || current.State.AckSequence != 0 ||
			current.State.Generation == previous.State.Generation || current.State.Generation == (domain.SyncCursorGeneration{}) {
			t.Fatalf("设备 %d 的已提交换代不符：%+v", previous.DeviceID, current)
		}
	}
	for index := 0; index < 2; index++ {
		assertRotated(before[index], partial[index])
	}
	if !slices.Equal(partial[2:], before[2:]) {
		t.Fatal("失败设备或其后记录发生了改变")
	}

	// 3. 移除测试故障，从已提交位置续跑，保留前批已取得的代次。
	execute("DROP TRIGGER fixture_restore_sync_batch_failure")
	remainder, err := batch.RunOnce(ctx, first.NextAfterDeviceID, 3, 2)
	wantRemainder := service.RestoredSyncDeviceBatchResult{Scanned: 2, Reset: 2, NextAfterDeviceID: 29, ReachedEnd: true}
	if err != nil || remainder != wantRemainder ||
		!slices.Equal(pages, []pageCall{{0, 2}, {20, 1}, {20, 2}, {29, 1}}) ||
		!slices.Equal(attempts, []uint64{19, 20, 21, 21, 29}) {
		t.Fatalf("续跑范围或结果不符：%+v，%v，页 %v，尝试 %v", remainder, err, pages, attempts)
	}
	finished := readStates()
	if !slices.Equal(finished[:2], partial[:2]) {
		t.Fatal("续跑重复修改了前批已提交的设备")
	}
	for index := 2; index < 4; index++ {
		assertRotated(before[index], finished[index])
	}
	if verified, err := service.VerifyRestoredSyncDeviceReset(ctx, before, 2, store.ListRestoredSyncDeviceCandidates); verified != 4 ||
		err != nil || !slices.Equal(readStates(), finished) {
		t.Fatalf("完成换代的目标未通过全量只读复核：%d，%v", verified, err)
	}
	t.Log("部分换代复核拒绝，全量换代后四条均通过，复核保持目标设备状态")
	t.Log("故障前两项已提交，游标停在设备 20；修复夹具后从 20 继续处理 21 和 29")
}

func TestRunRestoredSyncDeviceResetIntegration(t *testing.T) {
	// 1. 用独占库及临时文件记录写入前的清单，模拟第三台设备提交失败。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store, execute, readStates := prepareRestoredSyncDevicesIntegration(t, ctx)
	before := readStates()
	manifestPath := filepath.Join(t.TempDir(), "original-roster.json")
	save := func(_ context.Context, original []domain.RestoredSyncDeviceCandidate) error {
		if !slices.Equal(original, before) || !slices.Equal(readStates(), before) {
			t.Fatal("保存原始清单时目标已经发生变化")
		}
		return service.SaveRestoredSyncDeviceRoster(manifestPath, original)
	}
	first, err := service.RunRestoredSyncDeviceReset(ctx, nil, 2, 2, store.ListRestoredSyncDeviceCandidates, store.ResetRestoredSyncDevice, save)
	var databaseError *mysql.MySQLError
	if first != (service.RestoredSyncDeviceResetResult{Reset: 2}) ||
		!errors.As(err, &databaseError) || databaseError.Number != 1644 {
		t.Fatalf("故障应保留两次提交和原始错误：%+v，%v", first, err)
	}
	partial := readStates()
	if partial[0].State.Generation == before[0].State.Generation || partial[1].State.Generation == before[1].State.Generation ||
		!slices.Equal(partial[2:], before[2:]) {
		t.Fatal("故障前后的数据库换代范围不符")
	}

	// 2. 从临时文件重新读取首次清单，使用新的流程调用从头读取目标。
	saved, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	original, err := service.LoadRestoredSyncDeviceRoster(manifestPath)
	if err != nil || !slices.Equal(original, before) {
		t.Fatalf("仍应持有四条原始记录：%+v，%v", original, err)
	}
	execute("DROP TRIGGER fixture_restore_sync_batch_failure")
	retry, err := service.RunRestoredSyncDeviceReset(ctx, original, 2, 2, store.ListRestoredSyncDeviceCandidates, store.ResetRestoredSyncDevice, nil)
	if retry != (service.RestoredSyncDeviceResetResult{Reset: 4, Verified: 4}) || err != nil {
		t.Fatalf("重跑应按原始清单全量通过：%+v，%v", retry, err)
	}
	finished := readStates()
	for index, current := range finished {
		if current.State.Status != domain.SyncDeviceStatusResetRequired || current.State.AckSequence != 0 ||
			current.State.Generation == original[index].State.Generation || current.State.Generation == partial[index].State.Generation {
			t.Fatalf("设备 %d 的实际换代结果不符：%+v", current.DeviceID, current)
		}
	}
	if kept, err := os.ReadFile(manifestPath); err != nil || !bytes.Equal(saved, kept) {
		t.Fatalf("重跑修改了首次保存的清单：%v", err)
	}
	t.Log("首次保存 4 条原始记录，设备 21 故障前确认提交 2 条；重新载入同一清单后提交并读回 4 条，清单字节一致")
}
