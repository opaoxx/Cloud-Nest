package repository

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type fileReuseQuotaState struct {
	quota, used, reserved, sequence uint64
	updatedAt                       time.Time
}

func fileReuseQuotaTestFixture(t *testing.T, ctx context.Context) (*Store, domain.Actor, func() fileReuseQuotaState, func(quota, used, reserved uint64)) {
	t.Helper()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	readQuota := func() fileReuseQuotaState {
		t.Helper()
		var state fileReuseQuotaState
		if err := pool.QueryRowContext(ctx, `SELECT storage_quota,used_storage,reserved_storage,change_seq,updated_at
			FROM users WHERE id=?`, actor.UserID).
			Scan(&state.quota, &state.used, &state.reserved, &state.sequence, &state.updatedAt); err != nil {
			t.Fatal(err)
		}
		return state
	}
	setQuota := func(quota, used, reserved uint64) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, `UPDATE users SET storage_quota=?,used_storage=?,reserved_storage=?,
			updated_at='2026-11-17 08:00:00.123456' WHERE id=?`, quota, used, reserved, actor.UserID); err != nil {
			t.Fatal(err)
		}
	}
	return store, actor, readQuota, setQuota
}

func TestCheckUserFileReuseQuotaIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	store, actor, readQuota, setQuota := fileReuseQuotaTestFixture(t, ctx)
	check := func(sizeBytes uint64) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return txStore.CheckUserFileReuseQuota(txContext, sizeBytes)
		})
	}

	// 1. 在单连接的认证事务里检查边界，保留已用、预占和更新时间。
	for _, scenario := range []struct {
		name                        string
		quota, used, reserved, size uint64
		wantError                   error
	}{
		{"has remaining bytes", 100, 40, 20, 10, nil},
		{"exact remaining", 100, 40, 20, 40, nil},
		{"reservation counts", 100, 40, 20, 41, domain.ErrQuotaExceeded},
		{"full quota zero bytes", 100, 60, 40, 0, nil},
		{"zero quota zero bytes", 0, 0, 0, 0, nil},
		{"maximum exact remaining", math.MaxUint64, math.MaxUint64 - 2, 1, 1, nil},
		{"sum would overflow", math.MaxUint64, math.MaxUint64 - 2, 1, math.MaxUint64, domain.ErrQuotaExceeded},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			setQuota(scenario.quota, scenario.used, scenario.reserved)
			before := readQuota()
			err := check(scenario.size)
			if !errors.Is(err, scenario.wantError) || readQuota() != before {
				t.Fatal("配额判断或只读账目不符", err)
			}
		})
	}

	// 2. 通过正式上传预占用完剩余空间，后续预检看到最新账目。
	setQuota(100, 40, 20)
	if err := check(40); err != nil {
		t.Fatal("初始剩余四十字节未允许复用", err)
	}
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		return txStore.ReserveUserUploadSpace(txContext, 40)
	}); err != nil {
		t.Fatal("合成上传未预占剩余空间", err)
	}
	fullyReserved := readQuota()
	if fullyReserved.reserved != 60 || !errors.Is(check(1), domain.ErrQuotaExceeded) || readQuota() != fullyReserved {
		t.Fatal("预检未读到已提交的上传预占")
	}

	// 3. 检查事务范围、认证上下文和取消结果。
	if err := store.CheckUserFileReuseQuota(ctx, 0); err == nil {
		t.Fatal("直接调用通过了秒传配额检查")
	}
	if err := store.WithLoginUserTransaction(ctx, actor.UserID, func(txContext context.Context, txStore *Store) error {
		return txStore.CheckUserFileReuseQuota(txContext, 0)
	}); err == nil {
		t.Fatal("只有账号锁的登录事务通过了秒传配额检查")
	}
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		canceled, stop := context.WithCancel(txContext)
		stop()
		return txStore.CheckUserFileReuseQuota(canceled, 0)
	}); !errors.Is(err, context.Canceled) {
		t.Fatal("检查未传递取消原因", err)
	}
	if err := check(0); err != nil || readQuota() != fullyReserved {
		t.Fatal("配额用尽时零字节预检应保留原账目", err)
	}
	t.Log("锁内预检覆盖恰好满额、零字节、最大整数和已提交的上传预占，账号记录前后一致")
}

func TestSettleUserFileReuseQuotaIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	store, actor, readQuota, setQuota := fileReuseQuotaTestFixture(t, ctx)
	settle := func(sizeBytes uint64) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return txStore.SettleUserFileReuseQuota(txContext, sizeBytes)
		})
	}

	// 1. 按完整字节数结算到恰好满额，保留已有预占与变更序号。
	setQuota(100, 40, 20)
	before := readQuota()
	if err := settle(41); !errors.Is(err, domain.ErrQuotaExceeded) || readQuota() != before {
		t.Fatal("超出剩余四十字节时应保留原账目", err)
	}
	if err := settle(40); err != nil {
		t.Fatal("恰好满额的秒传结算失败", err)
	}
	full := readQuota()
	if full.used != 80 || full.quota != before.quota || full.reserved != before.reserved ||
		full.sequence != before.sequence || !full.updatedAt.After(before.updatedAt) {
		t.Fatalf("只应增加已用空间并更新账目时间，实际 %+v", full)
	}
	if err := settle(0); err != nil || readQuota() != full {
		t.Fatal("满额时零字节结算应保留原账目", err)
	}

	// 2. 外层失败时，即使事务里已经读到新金额，提交前的更新也会回滚。
	setQuota(100, 40, 20)
	before = readQuota()
	abort := errors.New("模拟创建流程后续失败")
	var pendingUsed uint64
	err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		if err := txStore.SettleUserFileReuseQuota(txContext, 10); err != nil {
			return err
		}
		err := txStore.database.WithContext(txContext).Raw("SELECT used_storage FROM users WHERE id=?", actor.UserID).Row().Scan(&pendingUsed)
		if err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) || pendingUsed != 50 || readQuota() != before {
		t.Fatalf("外层事务应回滚未提交的结算，事务内已用 %d，错误 %v", pendingUsed, err)
	}

	// 3. 旧预检通过后新增的上传预占，必须计入结算时的剩余空间。
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		return txStore.CheckUserFileReuseQuota(txContext, 40)
	}); err != nil {
		t.Fatal("初始配额预检失败", err)
	}
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		return txStore.ReserveUserUploadSpace(txContext, 1)
	}); err != nil {
		t.Fatal("合成上传未增加预占", err)
	}
	afterReservation := readQuota()
	if afterReservation.reserved != 21 || !errors.Is(settle(40), domain.ErrQuotaExceeded) || readQuota() != afterReservation {
		t.Fatal("结算应按最新预占拒绝超限并保留原账目")
	}

	// 4. 核对最大整数账目和事务调用边界。
	setQuota(math.MaxUint64, math.MaxUint64-2, 1)
	if err := settle(1); err != nil {
		t.Fatal("最大配额边界结算失败", err)
	}
	maximum := readQuota()
	if maximum.used != math.MaxUint64-1 || maximum.reserved != 1 {
		t.Fatalf("最大配额边界账目不符，实际 %+v", maximum)
	}
	if err := store.SettleUserFileReuseQuota(ctx, 0); err == nil {
		t.Fatal("直接调用通过了秒传结算")
	}
	if err := store.WithLoginUserTransaction(ctx, actor.UserID, func(txContext context.Context, txStore *Store) error {
		return txStore.SettleUserFileReuseQuota(txContext, 0)
	}); err == nil {
		t.Fatal("只有账号锁的登录事务通过了秒传结算")
	}
	if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
		canceled, stop := context.WithCancel(txContext)
		stop()
		return txStore.SettleUserFileReuseQuota(canceled, 0)
	}); !errors.Is(err, context.Canceled) || readQuota() != maximum {
		t.Fatal("取消时应传递取消原因并保留原账目", err)
	}
	t.Log("锁内结算覆盖恰好满额、零字节、最新预占、最大整数和外层回滚")
}
