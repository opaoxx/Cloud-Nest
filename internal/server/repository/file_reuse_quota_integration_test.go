package repository

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestCheckUserFileReuseQuotaIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)

	type quotaState struct {
		quota, used, reserved, sequence uint64
		updatedAt                       time.Time
	}
	readQuota := func() quotaState {
		t.Helper()
		var state quotaState
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
