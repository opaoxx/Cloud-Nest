package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type revokeSessionTxStub struct {
	revoke func(context.Context, uint64) error
}

func (transaction *revokeSessionTxStub) RevokeUserSession(ctx context.Context, targetSessionID uint64) error {
	return transaction.revoke(ctx, targetSessionID)
}

func TestRevokeSessionServiceCommit(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		actor    domain.Actor
		targetID uint64
	}{
		{name: "another session", actor: domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}, targetID: 19},
		{name: "current session", actor: domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}, targetID: 17},
		{name: "full unsigned ids", actor: domain.Actor{UserID: ^uint64(0), DeviceID: ^uint64(0), SessionID: ^uint64(0)}, targetID: ^uint64(0) - 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 使用独立事务上下文，记录撤销与提交顺序。
			type transactionKey struct{}
			ctx := context.Background()
			txContext := context.WithValue(ctx, transactionKey{}, true)
			var steps []string
			revoke := NewRevokeSessionService(func(received context.Context, actor domain.Actor, action func(context.Context, RevokeSessionTransaction) error) error {
				steps = append(steps, "begin")
				if received != ctx || actor != testCase.actor {
					t.Error("事务没有收到原上下文与准确 Actor")
				}
				transaction := &revokeSessionTxStub{revoke: func(received context.Context, targetID uint64) error {
					steps = append(steps, "revoke")
					if received != txContext || targetID != testCase.targetID {
						t.Error("目标撤销没有使用事务上下文或准确 ID")
					}
					return nil
				}}
				if err := action(txContext, transaction); err != nil {
					return err
				}
				steps = append(steps, "commit")
				return nil
			})

			// 2. 提交确认后取得结果，重复调用返回独立对象。
			first, err := revoke.Revoke(ctx, testCase.actor, testCase.targetID)
			if err != nil || first == nil || !first.Revoked || !reflect.DeepEqual(steps, []string{"begin", "revoke", "commit"}) {
				t.Fatal("提交结果或执行顺序错误", err)
			}
			first.Revoked = false
			steps = nil
			second, err := revoke.Revoke(ctx, testCase.actor, testCase.targetID)
			if err != nil || second == nil || !second.Revoked || first == second {
				t.Fatal("多次调用共享了可变结果", err)
			}
		})
	}
}

func TestRevokeSessionServiceFailures(t *testing.T) {
	failure := errors.New("fixture transaction fault")
	for _, testCase := range []struct {
		name                                       string
		entryError, revokeError, commitError, want error
		skipCallback, nilContext, nilStore         bool
		calls                                      int
	}{
		{name: "begin failure", entryError: failure, want: failure},
		{name: "caller missing session", entryError: domain.ErrSessionNotFound, want: domain.ErrSessionNotFound},
		{name: "caller revoked", entryError: domain.ErrSessionRevoked, want: domain.ErrSessionRevoked},
		{name: "caller expired", entryError: domain.ErrSessionExpired, want: domain.ErrSessionExpired},
		{name: "caller device revoked", entryError: domain.ErrDeviceRevoked, want: domain.ErrDeviceRevoked},
		{name: "account missing", entryError: domain.ErrAccountNotFound, want: domain.ErrAccountNotFound},
		{name: "account disabled", entryError: domain.ErrAccountDisabled, want: domain.ErrAccountDisabled},
		{name: "invalid caller data", entryError: domain.ErrInvalidSessionData, want: domain.ErrInvalidSessionData},
		{name: "callback omitted", skipCallback: true, want: domain.ErrInvalidSessionData},
		{name: "nil callback context", nilContext: true},
		{name: "nil callback store", nilStore: true},
		{name: "target not found", revokeError: domain.ErrSessionNotFound, want: domain.ErrTargetSessionNotFound, calls: 1},
		{name: "wrapped target not found", revokeError: fmt.Errorf("fixture target: %w", domain.ErrSessionNotFound), want: domain.ErrTargetSessionNotFound, calls: 1},
		{name: "target error preserved", revokeError: domain.ErrTargetSessionNotFound, want: domain.ErrTargetSessionNotFound, calls: 1},
		{name: "write failure", revokeError: failure, want: failure, calls: 1},
		{name: "invalid target data", revokeError: domain.ErrInvalidSessionData, want: domain.ErrInvalidSessionData, calls: 1},
		{name: "commit failure", commitError: failure, want: failure, calls: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			calls := 0
			revoke := NewRevokeSessionService(func(ctx context.Context, actor domain.Actor, action func(context.Context, RevokeSessionTransaction) error) error {
				if testCase.entryError != nil || testCase.skipCallback {
					return testCase.entryError
				}
				transaction := &revokeSessionTxStub{revoke: func(context.Context, uint64) error {
					calls++
					return testCase.revokeError
				}}
				if testCase.nilContext {
					ctx = nil
				}
				var txStore RevokeSessionTransaction = transaction
				if testCase.nilStore {
					txStore = nil
				}
				if err := action(ctx, txStore); err != nil {
					return err
				}
				return testCase.commitError
			})
			result, err := revoke.Revoke(context.Background(), domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}, 19)
			if result != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || calls != testCase.calls {
				t.Fatal("失败结果、错误链或调用次数错误", err, calls)
			}
			if errors.Is(err, domain.ErrTargetSessionNotFound) != (testCase.want == domain.ErrTargetSessionNotFound) {
				t.Fatal("调用者错误与目标错误混淆")
			}
			if testCase.want == domain.ErrTargetSessionNotFound && errors.Is(err, domain.ErrSessionNotFound) {
				t.Fatal("目标错误仍带有调用者会话失效的分类")
			}
		})
	}
}

func TestRevokeSessionServiceInput(t *testing.T) {
	for _, name := range []string{"nil context", "zero user", "zero device", "zero session", "zero target", "cancelled", "deadline exceeded"} {
		t.Run(name, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			targetID := uint64(19)
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
			case "zero target":
				targetID = 0
			case "cancelled":
				cancel()
			case "deadline exceeded":
				expired, release := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer release()
				callContext = expired
			}
			called := false
			revoke := NewRevokeSessionService(func(context.Context, domain.Actor, func(context.Context, RevokeSessionTransaction) error) error {
				called = true
				return nil
			})
			result, err := revoke.Revoke(callContext, actor, targetID)
			if result != nil || err == nil || called {
				t.Fatal("无效输入进入了事务")
			}
			if name == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("取消原因丢失")
			}
			if name == "deadline exceeded" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("期限原因丢失")
			}
		})
	}
	t.Run("missing transaction dependency", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("缺少事务依赖仍构造成功")
			}
		}()
		NewRevokeSessionService(nil)
	})
}

func TestRevokeSessionServiceContexts(t *testing.T) {
	for _, stage := range []string{"before revoke", "after revoke", "cancel before missing target classification", "revoke deadline", "after confirmed commit", "cancelled commit failure"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			txDuration := time.Second
			if stage == "revoke deadline" {
				txDuration = 20 * time.Millisecond
			}
			txContext, cancelTransaction := context.WithTimeout(ctx, txDuration)
			defer cancelTransaction()
			failure := errors.New("fixture commit fault")
			calls := 0
			revoke := NewRevokeSessionService(func(ctx context.Context, actor domain.Actor, action func(context.Context, RevokeSessionTransaction) error) error {
				if stage == "before revoke" {
					cancelTransaction()
				}
				transaction := &revokeSessionTxStub{revoke: func(received context.Context, targetID uint64) error {
					calls++
					if received != txContext {
						t.Error("撤销上下文被替换")
					}
					switch stage {
					case "revoke deadline":
						<-received.Done()
						return received.Err()
					case "after revoke":
						cancelTransaction()
					case "cancel before missing target classification":
						cancelTransaction()
						return domain.ErrSessionNotFound
					}
					return nil
				}}
				if err := action(txContext, transaction); err != nil {
					return err
				}
				cancel()
				if stage == "cancelled commit failure" {
					return failure
				}
				return nil
			})
			result, err := revoke.Revoke(ctx, domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}, 19)
			switch stage {
			case "after confirmed commit":
				if err != nil || result == nil || !result.Revoked || ctx.Err() == nil {
					t.Fatal("已确认提交结果丢失", err)
				}
			case "cancelled commit failure":
				if result != nil || !errors.Is(err, failure) {
					t.Fatal("提交故障被取消覆盖", err)
				}
			default:
				want := error(context.Canceled)
				if stage == "revoke deadline" {
					want = context.DeadlineExceeded
				}
				if result != nil || !errors.Is(err, want) || errors.Is(err, domain.ErrTargetSessionNotFound) {
					t.Fatal("事务取消原因或目标分类错误", err)
				}
			}
			if stage == "before revoke" && calls != 0 {
				t.Error("事务取消后进入撤销")
			}
		})
	}
}
