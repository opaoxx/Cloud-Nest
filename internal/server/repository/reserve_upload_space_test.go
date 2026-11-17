package repository

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// uploadQuotaRows 复用单行及行错误处理，提供当前配额五列。
type uploadQuotaRows struct{ *idempotencyRecordRows }

func (*uploadQuotaRows) Columns() []string {
	return []string{"id", "status", "quota_bytes", "used_bytes", "reserved_bytes"}
}

func TestReserveUserUploadSpaceResults(t *testing.T) {
	fault := errors.New("private quota failure")
	maximum := ^uint64(0)
	for _, fixture := range []struct {
		name                                                     string
		quota, used, reserved, size                              uint64
		rows                                                     int64
		status                                                   string
		missing, wrongOwner                                      bool
		queryError, rowError, updateError, outerError, wantError error
	}{
		{name: "ordinary reservation", quota: 100, used: 40, reserved: 20, size: 30, rows: 1},
		{name: "exact available", quota: 100, used: 40, reserved: 20, size: 40, rows: 1},
		{name: "insufficient space", quota: 100, used: 40, reserved: 20, size: 41, wantError: domain.ErrQuotaExceeded},
		{name: "zero quota zero file"},
		{name: "full quota zero file", quota: 100, used: 100},
		{name: "maximum reservation", quota: maximum, reserved: maximum - 1, size: 1, rows: 1},
		{name: "maximum request", quota: maximum, size: maximum, rows: 1},
		{name: "large used amount", quota: maximum, used: maximum - 10, reserved: 5, size: 5, rows: 1},
		{name: "addition would wrap", quota: maximum, reserved: 1, size: maximum, wantError: domain.ErrQuotaExceeded},
		{name: "used exceeds quota", quota: 10, used: 11, wantError: domain.ErrInvalidAccountData},
		{name: "reserved exceeds remaining", quota: 100, used: 40, reserved: 61, wantError: domain.ErrInvalidAccountData},
		{name: "existing sum would wrap", quota: maximum, used: maximum, reserved: 1, wantError: domain.ErrInvalidAccountData},
		{name: "disabled account", status: "DISABLED", wantError: domain.ErrAccountDisabled},
		{name: "unknown status", status: "UNKNOWN", wantError: domain.ErrInvalidAccountData},
		{name: "wrong owner", wrongOwner: true, wantError: domain.ErrInvalidAccountData},
		{name: "missing account", missing: true, wantError: domain.ErrAccountNotFound},
		{name: "query failure", queryError: fault, wantError: fault},
		{name: "row failure", rowError: fault, wantError: fault},
		{name: "update failure", quota: 100, size: 1, updateError: fault, wantError: fault},
		{name: "zero updated rows", quota: 100, size: 1, wantError: domain.ErrInvalidAccountData},
		{name: "multiple updated rows", quota: 100, size: 1, rows: 2, wantError: domain.ErrInvalidAccountData},
		{name: "outer rollback", quota: 100, size: 1, rows: 1, outerError: fault},
		{name: "commit failure", quota: 100, size: 1, rows: 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 准备已认证事务和包含完整无符号字段的配额行。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			if fixture.name == "commit failure" {
				script.commitError = fault
			}
			store := revokeSessionTestStore(t, actor, script)
			var transactionDeadline time.Time
			reads, writes := 0, 0
			script.quotaQuery = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				reads++
				expectedSQL := "SELECT id,status,storage_quota AS quota_bytes,used_storage AS used_bytes,reserved_storage AS reserved_bytes FROM users WHERE id = ? LIMIT ? FOR UPDATE"
				if strings.ReplaceAll(statement, "`", "") != expectedSQL || len(arguments) != 2 || arguments[0].Value != actor.UserID || arguments[1].Value != int64(1) {
					t.Error("用户配额投影、范围或当前读锁错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || !deadline.Equal(transactionDeadline) {
					t.Error("读取配额未服从事务期限")
				}
				status := fixture.status
				if status == "" {
					status = "ACTIVE"
				}
				values := []driver.Value{"7", status, strconv.FormatUint(fixture.quota, 10), strconv.FormatUint(fixture.used, 10), strconv.FormatUint(fixture.reserved, 10)}
				if fixture.missing {
					values = nil
				}
				if fixture.wrongOwner {
					values[0] = "8"
				}
				return &uploadQuotaRows{&idempotencyRecordRows{values: values, err: fixture.rowError}}, fixture.queryError
			}
			script.reserveStorage = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				writes++
				expectedSQL := "UPDATE users SET reserved_storage = ?, updated_at = UTC_TIMESTAMP(6) WHERE id = ? AND status = 'ACTIVE' AND storage_quota = ? AND used_storage = ? AND reserved_storage = ?"
				if statement != expectedSQL || len(arguments) != 5 {
					t.Fatal("预占更新字段或条件错误")
				}
				expected := []any{fixture.reserved + fixture.size, actor.UserID, fixture.quota, fixture.used, fixture.reserved}
				for index, argument := range arguments {
					if !reflect.DeepEqual(argument.Value, expected[index]) {
						t.Error("预占更新绑定值错误", index)
					}
				}
				if deadline, exists := ctx.Deadline(); !exists || !deadline.Equal(transactionDeadline) {
					t.Error("预占更新未服从事务期限")
				}
				return driver.RowsAffected(fixture.rows), fixture.updateError
			}
			// 2. 本方法失败交由外层回滚，后续故障也可撤销预占。
			err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
				transactionDeadline, _ = txContext.Deadline()
				if err := txStore.ReserveUserUploadSpace(txContext, fixture.size); err != nil {
					if errors.Is(err, domain.ErrQuotaExceeded) && err.Error() != "存储空间不足" {
						t.Error("配额不足提示包含内部数据")
					}
					return err
				}
				return fixture.outerError
			})
			// 3. 核对结果和精确事务顺序，零字节省去更新。
			wantError := fixture.wantError
			if fixture.outerError != nil || script.commitError != nil {
				wantError = fault
			}
			if (wantError == nil && err != nil) || (wantError != nil && !errors.Is(err, wantError)) || reads != 1 {
				t.Fatal("预占结果或错误链错误", err)
			}
			expectedSequence := "begin,lock,device,session,quota"
			if writes != 0 {
				expectedSequence += ",reserve"
			}
			if wantError == nil || script.commitError != nil {
				expectedSequence += ",commit"
			} else {
				expectedSequence += ",rollback"
			}
			if script.sequence() != expectedSequence || writes > 1 || (fixture.size == 0 && writes != 0) ||
				(errors.Is(err, domain.ErrQuotaExceeded) && writes != 0) || (fixture.size > 0 && wantError == nil && writes != 1) {
				t.Fatal("预占更新次数或事务结束错误", script.sequence())
			}
		})
	}
}

func TestReserveUserUploadSpaceBoundary(t *testing.T) {
	for _, mode := range []string{"ordinary store", "bound store without transaction", "login transaction", "nil context", "wrong owner", "zero device", "zero session", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			action := func(ctx context.Context, txStore *Store) error {
				var wantError error
				switch mode {
				case "nil context":
					ctx = nil
				case "wrong owner":
					txStore.loginUserID, wantError = 8, domain.ErrInvalidSessionData
				case "zero device":
					txStore.authenticatedActor.DeviceID, wantError = 0, domain.ErrInvalidSessionData
				case "zero session":
					txStore.authenticatedActor.SessionID, wantError = 0, domain.ErrInvalidSessionData
				case "cancelled":
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					ctx, wantError = cancelled, context.Canceled
				}
				err := txStore.ReserveUserUploadSpace(ctx, 1)
				if err == nil || (wantError != nil && !errors.Is(err, wantError)) {
					t.Fatal("无效预占调用未正确拒绝", err)
				}
				return err
			}
			var err error
			switch mode {
			case "ordinary store":
				err = action(context.Background(), store)
			case "bound store without transaction":
				err = action(context.Background(), &Store{database: store.database, loginUserID: 7, authenticatedActor: &actor})
			case "login transaction":
				err = store.WithLoginUserTransaction(context.Background(), 7, action)
			default:
				err = store.WithUserTransaction(context.Background(), actor, action)
			}
			if err == nil || strings.Contains(script.sequence(), "quota") || strings.Contains(script.sequence(), "reserve") || strings.Contains(script.sequence(), "commit") {
				t.Fatal("无效调用触发配额访问或提交", err, script.sequence())
			}
		})
	}
}

func TestReserveUserUploadSpaceCancellation(t *testing.T) {
	for _, phase := range []string{"quota deadline", "after quota", "missing quota cancelled", "quota failure cancelled", "zero-byte cancelled", "update deadline", "after update", "update failure cancelled"} {
		t.Run(phase, func(t *testing.T) {
			// 1. 在当前读和更新阶段分别注入取消，取消原因优先。
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			script.quotaQuery = func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if phase == "quota deadline" {
					<-queryContext.Done()
					return nil, queryContext.Err()
				}
				if phase != "update deadline" && phase != "after update" && phase != "update failure cancelled" {
					cancel()
				}
				if phase == "quota failure cancelled" {
					return nil, errors.New("private cancelled quota")
				}
				values := []driver.Value{"7", "ACTIVE", "100", "20", "10"}
				if phase == "missing quota cancelled" {
					values = nil
				}
				return &uploadQuotaRows{&idempotencyRecordRows{values: values}}, nil
			}
			script.reserveStorage = func(updateContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if phase == "update deadline" {
					<-updateContext.Done()
					return nil, updateContext.Err()
				}
				cancel()
				if phase == "update failure cancelled" {
					return nil, errors.New("private cancelled reservation")
				}
				return driver.RowsAffected(1), nil
			}
			// 2. 失败交回事务入口并等待真实 database/sql 回滚。
			size := uint64(1)
			if phase == "zero-byte cancelled" {
				size = 0
			}
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				return txStore.ReserveUserUploadSpace(txContext, size)
			})
			wantError := error(context.Canceled)
			if strings.HasSuffix(phase, "deadline") {
				wantError = context.DeadlineExceeded
			}
			if !errors.Is(err, wantError) || strings.Contains(script.sequence(), "commit") {
				t.Fatal("预占取消原因丢失或仍提交", err, script.sequence())
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("预占取消后未回滚")
			}
		})
	}
}
