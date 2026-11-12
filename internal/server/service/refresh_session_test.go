package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type refreshSessionTxStub struct {
	rotate func(context.Context, [sha256.Size]byte) (*domain.VerifiedRefreshSession, error)
}

func (transaction *refreshSessionTxStub) RotateRefreshToken(ctx context.Context, hash [sha256.Size]byte) (*domain.VerifiedRefreshSession, error) {
	return transaction.rotate(ctx, hash)
}

func refreshSessionFixture() (string, domain.VerifiedRefreshSession) {
	raw, hash := GenerateRefreshToken()
	return raw, domain.VerifiedRefreshSession{
		Session:         domain.LoginSession{ID: 17, UserID: 7, DeviceID: 11, ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)},
		TokenGeneration: 1, TokenHash: hash,
	}
}

func TestRefreshSessionServiceCommitAndCopies(t *testing.T) {
	raw, verified := refreshSessionFixture()
	verified.Session.ID, verified.Session.UserID, verified.Session.DeviceID = ^uint64(0), ^uint64(0), ^uint64(0)
	verified.TokenGeneration = ^uint64(0) - 1
	ctx := context.Background()
	type transactionKey struct{}
	txContext := context.WithValue(ctx, transactionKey{}, true)
	current := verified
	current.Session.ExpiresAt = verified.Session.ExpiresAt.Add(time.Minute).In(time.FixedZone("UTC+8", 8*60*60))
	var steps []string
	var storedHash [sha256.Size]byte
	var returned domain.VerifiedRefreshSession
	transaction := &refreshSessionTxStub{rotate: func(received context.Context, hash [sha256.Size]byte) (*domain.VerifiedRefreshSession, error) {
		steps = append(steps, "rotate")
		if received != txContext {
			t.Error("轮换未使用事务上下文")
		}
		storedHash = hash
		returned = current
		returned.TokenHash, returned.TokenGeneration = hash, current.TokenGeneration+1
		return &returned, nil
	}}
	refresher := NewRefreshSessionService(func(received context.Context, receivedRaw string) (*domain.VerifiedRefreshSession, error) {
		steps = append(steps, "verify")
		if received != ctx || receivedRaw != raw {
			t.Error("校验未收到原上下文和原文")
		}
		return &verified, nil
	}, func(received context.Context, candidate domain.VerifiedRefreshSession, action func(context.Context, RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
		steps = append(steps, "begin")
		if received != ctx || candidate != verified {
			t.Error("刷新事务未收到初次校验副本")
		}
		if err := action(txContext, transaction, current); err != nil {
			return err
		}
		steps = append(steps, "commit")
		return nil
	})
	generate := refresher.generateRefreshToken
	refresher.generateRefreshToken = func() (string, [sha256.Size]byte) {
		steps = append(steps, "generate")
		return generate()
	}
	result, err := refresher.Refresh(ctx, raw)
	if err != nil || result == nil || result.Session.ID != ^uint64(0) || result.Session.UserID != ^uint64(0) || result.Session.DeviceID != ^uint64(0) ||
		!result.Session.ExpiresAt.Equal(current.Session.ExpiresAt) || result.Session.ExpiresAt.Location() != time.UTC ||
		result.TokenGeneration != ^uint64(0) || len(result.RefreshToken) != 43 || sha256.Sum256([]byte(result.RefreshToken)) != storedHash {
		t.Fatal("提交结果、新原文摘要或锁内期限错误", err)
	}
	if !reflect.DeepEqual(steps, []string{"verify", "generate", "begin", "rotate", "commit"}) {
		t.Fatal("执行顺序错误", steps)
	}
	returned.Session.ID, verified.Session.ID = 1, 2
	if result.Session.ID != ^uint64(0) {
		t.Fatal("结果持有了依赖返回对象的可变引用")
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), result.RefreshToken) || strings.Contains(string(encoded), "RefreshToken") || strings.Contains(string(encoded), "TokenHash") {
		t.Fatal("内部结果序列化了刷新原文或摘要")
	}
}

func TestRefreshSessionServiceFailures(t *testing.T) {
	fault := errors.New("fixture dependency failure")
	for _, testCase := range []struct {
		name string
		want error
	}{
		{name: "invalid credential", want: domain.ErrInvalidRefreshToken},
		{name: "disabled account", want: domain.ErrAccountDisabled},
		{name: "verification fault", want: fault},
		{name: "nil snapshot", want: domain.ErrInvalidSessionData},
		{name: "zero user", want: domain.ErrInvalidSessionData},
		{name: "zero session", want: domain.ErrInvalidSessionData},
		{name: "zero device", want: domain.ErrInvalidSessionData},
		{name: "zero generation", want: domain.ErrInvalidSessionData},
		{name: "maximum generation", want: domain.ErrInvalidSessionData},
		{name: "wrong old hash", want: domain.ErrInvalidSessionData},
		{name: "zero expiry", want: domain.ErrInvalidSessionData},
		{name: "expired snapshot", want: domain.ErrSessionExpired},
		{name: "transaction fault", want: fault},
		{name: "missing callback", want: domain.ErrInvalidSessionData},
		{name: "nil transaction context"},
		{name: "nil transaction store"},
		{name: "current wrong user", want: domain.ErrInvalidSessionData},
		{name: "current wrong session", want: domain.ErrInvalidSessionData},
		{name: "current wrong device", want: domain.ErrInvalidSessionData},
		{name: "current wrong generation", want: domain.ErrInvalidSessionData},
		{name: "current wrong hash", want: domain.ErrInvalidSessionData},
		{name: "current zero expiry", want: domain.ErrInvalidSessionData},
		{name: "current expired", want: domain.ErrSessionExpired},
		{name: "rotation fault", want: fault},
		{name: "digest conflict", want: domain.ErrRefreshTokenConflict},
		{name: "stale credential", want: domain.ErrInvalidRefreshToken},
		{name: "nil rotated", want: domain.ErrInvalidSessionData},
		{name: "rotated wrong user", want: domain.ErrInvalidSessionData},
		{name: "rotated wrong session", want: domain.ErrInvalidSessionData},
		{name: "rotated wrong device", want: domain.ErrInvalidSessionData},
		{name: "rotated wrong generation", want: domain.ErrInvalidSessionData},
		{name: "rotated wrong hash", want: domain.ErrInvalidSessionData},
		{name: "rotated wrong expiry", want: domain.ErrInvalidSessionData},
		{name: "commit fault", want: fault},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw, original := refreshSessionFixture()
			snapshot := original
			var generated, begun, rotated bool
			refresher := NewRefreshSessionService(func(context.Context, string) (*domain.VerifiedRefreshSession, error) {
				switch testCase.name {
				case "invalid credential", "disabled account", "verification fault":
					return nil, testCase.want
				case "nil snapshot":
					return nil, nil
				case "zero user":
					snapshot.Session.UserID = 0
				case "zero session":
					snapshot.Session.ID = 0
				case "zero device":
					snapshot.Session.DeviceID = 0
				case "zero generation":
					snapshot.TokenGeneration = 0
				case "maximum generation":
					snapshot.TokenGeneration = ^uint64(0)
				case "wrong old hash":
					snapshot.TokenHash = [sha256.Size]byte{}
				case "zero expiry":
					snapshot.Session.ExpiresAt = time.Time{}
				case "expired snapshot":
					snapshot.Session.ExpiresAt = time.Now().Add(-time.Hour)
				}
				return &snapshot, nil
			}, func(ctx context.Context, candidate domain.VerifiedRefreshSession, action func(context.Context, RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
				begun = true
				if candidate != original {
					t.Error("异常初次校验结果进入了事务")
				}
				if testCase.name == "transaction fault" {
					return fault
				}
				if testCase.name == "missing callback" {
					return nil
				}
				current := candidate
				switch testCase.name {
				case "current wrong user":
					current.Session.UserID++
				case "current wrong session":
					current.Session.ID++
				case "current wrong device":
					current.Session.DeviceID++
				case "current wrong generation":
					current.TokenGeneration++
				case "current wrong hash":
					current.TokenHash = [sha256.Size]byte{}
				case "current zero expiry":
					current.Session.ExpiresAt = time.Time{}
				case "current expired":
					current.Session.ExpiresAt = time.Now().Add(-time.Hour)
				}
				transaction := &refreshSessionTxStub{}
				transaction.rotate = func(_ context.Context, hash [sha256.Size]byte) (*domain.VerifiedRefreshSession, error) {
					rotated = true
					if testCase.name == "rotation fault" || testCase.name == "digest conflict" || testCase.name == "stale credential" {
						return nil, testCase.want
					}
					if testCase.name == "nil rotated" {
						return nil, nil
					}
					result := current
					result.TokenGeneration++
					result.TokenHash = hash
					switch testCase.name {
					case "rotated wrong user":
						result.Session.UserID++
					case "rotated wrong session":
						result.Session.ID++
					case "rotated wrong device":
						result.Session.DeviceID++
					case "rotated wrong generation":
						result.TokenGeneration++
					case "rotated wrong hash":
						result.TokenHash = original.TokenHash
					case "rotated wrong expiry":
						result.Session.ExpiresAt = result.Session.ExpiresAt.Add(time.Minute)
					}
					return &result, nil
				}
				var callbackError error
				switch testCase.name {
				case "nil transaction context":
					callbackError = action(nil, transaction, current)
				case "nil transaction store":
					callbackError = action(ctx, nil, current)
				default:
					callbackError = action(ctx, transaction, current)
				}
				if callbackError != nil {
					return callbackError
				}
				if testCase.name == "commit fault" {
					return fault
				}
				t.Error("失败用例通过了事务回调")
				return nil
			})
			generate := refresher.generateRefreshToken
			refresher.generateRefreshToken = func() (string, [sha256.Size]byte) { generated = true; return generate() }
			result, err := refresher.Refresh(context.Background(), raw)
			if result != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) {
				t.Fatal("失败结果或错误链不正确", err)
			}
			if strings.Contains(err.Error(), raw) {
				t.Error("错误包含旧刷新原文")
			}
			if !begun && (generated || rotated) {
				t.Error("校验失败后仍执行了后续步骤")
			}
			if strings.HasPrefix(testCase.name, "current ") && rotated {
				t.Error("异常锁内资料进入了轮换")
			}
		})
	}
}

func TestRefreshSessionServiceContexts(t *testing.T) {
	for _, stage := range []string{"nil context", "before verify", "verify deadline", "after verify", "after generate", "before rotate", "after rotate", "after commit", "cancelled commit failure"} {
		t.Run(stage, func(t *testing.T) {
			raw, verified := refreshSessionFixture()
			deadline := time.Second
			if stage == "verify deadline" {
				deadline = 20 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			commitFailure := errors.New("fixture commit fault")
			var checked, began bool
			refresher := NewRefreshSessionService(func(context.Context, string) (*domain.VerifiedRefreshSession, error) {
				checked = true
				if stage == "verify deadline" {
					<-ctx.Done()
				}
				if stage == "after verify" {
					cancel()
				}
				return &verified, nil
			}, func(ctx context.Context, _ domain.VerifiedRefreshSession, action func(context.Context, RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
				began = true
				if stage == "before rotate" {
					cancel()
				}
				transaction := &refreshSessionTxStub{rotate: func(_ context.Context, hash [sha256.Size]byte) (*domain.VerifiedRefreshSession, error) {
					result := verified
					result.TokenHash, result.TokenGeneration = hash, verified.TokenGeneration+1
					if stage == "after rotate" {
						cancel()
					}
					return &result, nil
				}}
				if err := action(ctx, transaction, verified); err != nil {
					return err
				}
				cancel()
				if stage == "cancelled commit failure" {
					return commitFailure
				}
				return nil
			})
			generate := refresher.generateRefreshToken
			refresher.generateRefreshToken = func() (string, [sha256.Size]byte) {
				if stage == "after generate" {
					cancel()
				}
				return generate()
			}
			if stage == "before verify" {
				cancel()
			}
			var callContext context.Context = ctx
			if stage == "nil context" {
				callContext = nil
			}
			result, err := refresher.Refresh(callContext, raw)
			switch stage {
			case "after commit":
				if err != nil || result == nil || ctx.Err() == nil {
					t.Fatal("确认提交后的结果丢失", err)
				}
			case "cancelled commit failure":
				if result != nil || !errors.Is(err, commitFailure) {
					t.Fatal("提交故障被后续取消覆盖", err)
				}
			case "nil context":
				if result != nil || err == nil || checked || began {
					t.Fatal("nil 上下文执行了依赖")
				}
			case "verify deadline":
				if result != nil || !errors.Is(err, context.DeadlineExceeded) || began {
					t.Fatal("校验到期后仍继续事务", err)
				}
			default:
				if result != nil || !errors.Is(err, context.Canceled) {
					t.Fatal("取消后仍返回结果", err)
				}
			}
		})
	}
}

func TestRefreshSessionServiceDependencies(t *testing.T) {
	for _, missing := range []string{"verify", "transaction"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖时未拒绝构造")
				}
			}()
			verify := func(context.Context, string) (*domain.VerifiedRefreshSession, error) { return nil, nil }
			transaction := func(context.Context, domain.VerifiedRefreshSession, func(context.Context, RefreshSessionTransaction, domain.VerifiedRefreshSession) error) error {
				return nil
			}
			if missing == "verify" {
				verify = nil
			} else {
				transaction = nil
			}
			NewRefreshSessionService(verify, transaction)
		})
	}
}
