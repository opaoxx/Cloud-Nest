package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type logoutTxStub struct{ revoke func(context.Context) error }

func (transaction *logoutTxStub) RevokeCurrentSession(ctx context.Context) error {
	return transaction.revoke(ctx)
}

func TestLogoutServiceCommit(t *testing.T) {
	actor := domain.Actor{UserID: ^uint64(0), DeviceID: ^uint64(0), SessionID: ^uint64(0)}
	ctx := context.Background()
	type transactionKey struct{}
	txContext := context.WithValue(ctx, transactionKey{}, true)
	var steps []string
	logout := NewLogoutService(func(received context.Context, receivedActor domain.Actor, action func(context.Context, LogoutTransaction) error) error {
		steps = append(steps, "begin")
		if received != ctx || receivedActor != actor {
			t.Error("事务未收到原始上下文和完整 Actor")
		}
		transaction := &logoutTxStub{revoke: func(received context.Context) error {
			steps = append(steps, "revoke")
			if received != txContext {
				t.Error("撤销未使用事务上下文")
			}
			return nil
		}}
		if err := action(txContext, transaction); err != nil {
			return err
		}
		steps = append(steps, "commit")
		return nil
	})
	first, err := logout.Logout(ctx, actor)
	if err != nil || first == nil || !first.Revoked || !reflect.DeepEqual(steps, []string{"begin", "revoke", "commit"}) {
		t.Fatal("提交结果或执行顺序错误", err)
	}
	first.Revoked = false
	steps = nil
	second, err := logout.Logout(ctx, actor)
	if err != nil || second == nil || !second.Revoked || first == second {
		t.Fatal("调用结果相互共享可变状态", err)
	}
}

func TestLogoutServiceFailures(t *testing.T) {
	fault := errors.New("fixture transaction failure")
	for _, testCase := range []struct {
		name        string
		want        error
		revokeCalls int
	}{
		{name: "begin failure", want: fault},
		{name: "revoked before callback", want: domain.ErrSessionRevoked},
		{name: "missing session", want: domain.ErrSessionNotFound},
		{name: "disabled account", want: domain.ErrAccountDisabled},
		{name: "callback omitted", want: domain.ErrInvalidSessionData},
		{name: "nil transaction context"},
		{name: "nil transaction store"},
		{name: "revoke failure", want: fault, revokeCalls: 1},
		{name: "revoke no match", want: domain.ErrSessionRevoked, revokeCalls: 1},
		{name: "commit failure", want: fault, revokeCalls: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			calls := 0
			logout := NewLogoutService(func(ctx context.Context, actor domain.Actor, action func(context.Context, LogoutTransaction) error) error {
				switch testCase.name {
				case "begin failure", "revoked before callback", "missing session", "disabled account":
					return testCase.want
				case "callback omitted":
					return nil
				}
				transaction := &logoutTxStub{revoke: func(context.Context) error {
					calls++
					if testCase.name == "revoke failure" || testCase.name == "revoke no match" {
						return testCase.want
					}
					return nil
				}}
				var callbackError error
				switch testCase.name {
				case "nil transaction context":
					callbackError = action(nil, transaction)
				case "nil transaction store":
					callbackError = action(ctx, nil)
				default:
					callbackError = action(ctx, transaction)
				}
				if callbackError != nil {
					return callbackError
				}
				if testCase.name == "commit failure" {
					return fault
				}
				t.Error("失败用例通过了事务")
				return nil
			})
			result, err := logout.Logout(context.Background(), domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17})
			if result != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || calls != testCase.revokeCalls {
				t.Fatal("失败结果、错误链或调用次数错误", err, calls)
			}
		})
	}
}

func TestLogoutServiceInput(t *testing.T) {
	for _, name := range []string{"nil context", "zero user", "zero device", "zero session", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			switch name {
			case "nil context":
				callContext = nil
			case "zero user":
				actor.UserID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero session":
				actor.SessionID = 0
			case "cancelled":
				cancel()
			}
			called := false
			logout := NewLogoutService(func(context.Context, domain.Actor, func(context.Context, LogoutTransaction) error) error {
				called = true
				return nil
			})
			result, err := logout.Logout(callContext, actor)
			if result != nil || err == nil || called {
				t.Fatal("无效输入进入了事务")
			}
			if name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("取消原因丢失")
			}
		})
	}
	t.Run("missing dependency", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("缺少事务依赖仍构造成功")
			}
		}()
		NewLogoutService(nil)
	})
}

func TestLogoutServiceContexts(t *testing.T) {
	for _, stage := range []string{"before revoke", "after revoke", "revoke deadline", "after confirmed commit", "cancelled commit failure"} {
		t.Run(stage, func(t *testing.T) {
			deadline := time.Second
			if stage == "revoke deadline" {
				deadline = 20 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			fault := errors.New("fixture commit failure")
			calls := 0
			logout := NewLogoutService(func(ctx context.Context, actor domain.Actor, action func(context.Context, LogoutTransaction) error) error {
				if stage == "before revoke" {
					cancel()
				}
				transaction := &logoutTxStub{revoke: func(ctx context.Context) error {
					calls++
					if stage == "revoke deadline" {
						<-ctx.Done()
						return ctx.Err()
					}
					if stage == "after revoke" {
						cancel()
					}
					return nil
				}}
				if err := action(ctx, transaction); err != nil {
					return err
				}
				cancel()
				if stage == "cancelled commit failure" {
					return fault
				}
				return nil
			})
			result, err := logout.Logout(ctx, domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17})
			switch stage {
			case "after confirmed commit":
				if err != nil || result == nil || !result.Revoked || ctx.Err() == nil {
					t.Fatal("已确认提交的结果丢失", err)
				}
			case "cancelled commit failure":
				if result != nil || !errors.Is(err, fault) {
					t.Fatal("提交故障被取消覆盖", err)
				}
			default:
				want := error(context.Canceled)
				if stage == "revoke deadline" {
					want = context.DeadlineExceeded
				}
				if result != nil || !errors.Is(err, want) {
					t.Fatal("取消或期限错误", err)
				}
			}
			if stage == "before revoke" && calls != 0 {
				t.Error("取消后进入撤销写入")
			}
		})
	}
}
