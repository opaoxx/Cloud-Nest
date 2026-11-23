package repository

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestConfirmSyncChangesInTransactionIntegration(t *testing.T) {
	// 1. 在独占库准备已确认到 17 的合成设备和仍保留的 18～22 号事件。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	installChangeEventFixtureTable(t, ctx, pool)
	_, deviceDDL, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found {
		t.Fatal("未找到正式同步设备表")
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(deviceDDL)
	generation := domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe}
	execute(`INSERT INTO sync_devices(device_id,user_id,status,ack_sequence,cursor_generation,last_seen_at,updated_at)
		VALUES(19,1,'ACTIVE',17,?,'2020-01-01','2020-01-01')`, generation[:])
	execute("UPDATE users SET change_seq=22 WHERE id=1")
	for sequence := uint64(18); sequence <= 22; sequence++ {
		execute("INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload) VALUES(1,?,12,1,'DIRECTORY_CREATED',?)",
			sequence, `{"entry_id":"12","parent_id":"11","name":"合成目录","revision":"1"}`)
	}
	confirm := func(sequence uint64) (uint64, error) {
		verified := domain.SyncChangeCursorPosition{UserID: actor.UserID, DeviceID: actor.DeviceID, Generation: generation, Sequence: sequence}
		var ack uint64
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, transaction *Store) error {
			var confirmationError error
			ack, confirmationError = service.ConfirmSyncChangesInTransaction(txContext, actor, &verified, transaction)
			return confirmationError
		})
		if err != nil {
			return 0, err
		}
		return ack, nil
	}
	type confirmationState struct {
		position          uint64
		lastSeen, updated time.Time
	}
	readState := func() confirmationState {
		t.Helper()
		var state confirmationState
		if err := pool.QueryRowContext(ctx, "SELECT ack_sequence,last_seen_at,updated_at FROM sync_devices WHERE device_id=19").
			Scan(&state.position, &state.lastSeen, &state.updated); err != nil {
			t.Fatal("读取合成确认记录失败", err)
		}
		return state
	}
	before := readState()
	if ack, err := confirm(17); err != nil || ack != 17 {
		t.Fatalf("等值确认失败：位置 %d，错误 %v", ack, err)
	}
	if active := readState(); active.position != 17 || !active.lastSeen.After(before.lastSeen) || !active.updated.Equal(active.lastSeen) {
		t.Fatalf("等值确认应只记录活动：%+v", active)
	}

	// 2. 由两条连接并发确认 19 与 22，最终位置只能是 22。
	type confirmationResult struct {
		requested, acknowledged uint64
		err                     error
	}
	gate := make(chan struct{})
	results := make(chan confirmationResult, 2)
	for _, sequence := range []uint64{19, 22} {
		go func() {
			<-gate
			ack, err := confirm(sequence)
			results <- confirmationResult{requested: sequence, acknowledged: ack, err: err}
		}()
	}
	close(gate)
	for range 2 {
		result := <-results
		if result.err != nil || (result.acknowledged != result.requested && result.acknowledged != 22) {
			t.Errorf("并发确认 %d 返回位置 %d，错误 %v", result.requested, result.acknowledged, result.err)
		}
	}
	finished := readState()
	if finished.position != 22 {
		t.Fatalf("并发确认后位置为 %d，期望 22", finished.position)
	}

	// 3. 合法较旧位置返回现值，裁剪边界外的旧位置失败，两者都不更新设备。
	if ack, err := confirm(18); err != nil || ack != 22 || readState() != finished {
		t.Fatalf("较旧确认应只返回当前位置：位置 %d，错误 %v", ack, err)
	}
	if ack, err := confirm(16); !errors.Is(err, domain.ErrSyncSnapshotExpired) || ack != 0 || readState() != finished {
		t.Fatalf("超出保留范围的确认不符：位置 %d，错误 %v", ack, err)
	}
}
