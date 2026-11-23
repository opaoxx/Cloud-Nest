package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

type countedSyncBaselineTransaction struct {
	*Store
	activations *atomic.Int32
}

func (transaction countedSyncBaselineTransaction) ActivateSyncDeviceBaseline(ctx context.Context, generation domain.SyncCursorGeneration, cutoff uint64) (bool, error) {
	updated, err := transaction.Store.ActivateSyncDeviceBaseline(ctx, generation, cutoff)
	if updated {
		transaction.activations.Add(1)
	}
	return updated, err
}

func TestSyncSnapshotBaselineConfirmationServiceIntegration(t *testing.T) {
	// 1. 在独占库建立待确认设备和可续读日志，使用真实签发器和账号事务。
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
	execute("UPDATE users SET change_seq=22 WHERE id=?", actor.UserID)
	for sequence := uint64(18); sequence <= 22; sequence++ {
		execute("INSERT INTO change_events(user_id,`sequence`,entry_id,revision,event_type,payload) VALUES(?,?,11,1,'FIXTURE_CHANGE','{}')", actor.UserID, sequence)
	}
	binding := service.SyncSnapshotCacheBinding{
		SnapshotID: uuid.MustParse("052dc4f7-f4c1-46b3-a509-bf96992f6580"), UserID: actor.UserID, DeviceID: actor.DeviceID,
		Generation: domain.SyncCursorGeneration{0xff, 0, 7, 0, 19, 0, 21, 9, 11, 17, 21, 41, 63, 26, 33, 0xe1},
	}
	execute("INSERT INTO sync_devices(device_id,user_id,cursor_generation,last_seen_at) VALUES(?,?,?,'2020-01-01')",
		actor.DeviceID, actor.UserID, binding.Generation[:])
	clock := func() time.Time { return time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC) }
	masterKey := bytes.Repeat([]byte{0x28}, sha256.Size)
	signer, err := service.NewSyncSnapshotBaselineSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewSyncSnapshotBaselineVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(binding, 17, clock().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var activations atomic.Int32
	newConfirmer := func(afterConfirmation error) *service.SyncSnapshotBaselineConfirmationService {
		return service.NewSyncSnapshotBaselineConfirmationService(verifier,
			func(parent context.Context, identity domain.Actor, confirm func(context.Context, service.SyncSnapshotBaselineConfirmationTransaction) error) error {
				return store.WithUserTransaction(parent, identity, func(txContext context.Context, txStore *Store) error {
					if err := confirm(txContext, countedSyncBaselineTransaction{Store: txStore, activations: &activations}); err != nil {
						return err
					}
					return afterConfirmation
				})
			}, clock)
	}
	type positionSnapshot struct {
		status    domain.SyncDeviceStatus
		position  uint64
		seenAt    time.Time
		updatedAt time.Time
	}
	readPosition := func() positionSnapshot {
		t.Helper()
		var snapshot positionSnapshot
		if err := pool.QueryRowContext(ctx, "SELECT status,ack_sequence,last_seen_at,updated_at FROM sync_devices WHERE device_id=?", actor.DeviceID).
			Scan(&snapshot.status, &snapshot.position, &snapshot.seenAt, &snapshot.updatedAt); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}

	// 2. 强制在更新之后回滚，再用两条并发请求确认同一凭据。
	pending := readPosition()
	rollback := errors.New("fixture rollback after baseline activation")
	position, err := newConfirmer(rollback).Confirm(ctx, actor, binding.SnapshotID, token)
	if position != 0 || !errors.Is(err, rollback) || activations.Load() != 1 || readPosition() != pending {
		t.Fatalf("回滚未隐藏位置或恢复设备原行：%d，错误 %v", position, err)
	}
	confirmer := newConfirmer(nil)
	start := make(chan struct{})
	type confirmResult struct {
		position uint64
		err      error
	}
	results := make(chan confirmResult, 2)
	for range 2 {
		go func() {
			<-start
			position, err := confirmer.Confirm(ctx, actor, binding.SnapshotID, token)
			results <- confirmResult{position: position, err: err}
		}()
	}
	close(start)
	for range 2 {
		result := <-results
		if result.position != 17 || result.err != nil {
			t.Errorf("并发确认未返回提交的位置：%+v", result)
		}
	}
	if active := readPosition(); activations.Load() != 2 || active.status != domain.SyncDeviceStatusActive || active.position != 17 {
		t.Fatalf("并发调用应只激活一次，状态 %+v，累计更新数 %d", active, activations.Load())
	}

	// 3. 新服务实例可凭原签名确认已有位置，撤销会话后由账号事务拒绝。
	execute("UPDATE sync_devices SET ack_sequence=20 WHERE device_id=?", actor.DeviceID)
	advanced := readPosition()
	position, err = newConfirmer(nil).Confirm(ctx, actor, binding.SnapshotID, token)
	if position != 20 || err != nil || readPosition() != advanced || activations.Load() != 2 {
		t.Fatalf("重试未保留已推进的位置：%d，错误 %v", position, err)
	}
	execute("UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", actor.SessionID)
	position, err = confirmer.Confirm(ctx, actor, binding.SnapshotID, token)
	if position != 0 || !errors.Is(err, domain.ErrSessionRevoked) || readPosition() != advanced {
		t.Fatalf("会话撤销后未拒绝确认：%d，错误 %v", position, err)
	}
}
