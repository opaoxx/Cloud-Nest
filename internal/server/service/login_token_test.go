package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func loginTokenTestSession() *domain.LoginSessionResult {
	return &domain.LoginSessionResult{
		Device: domain.LoginDevice{ID: 13, UserID: 7, InstallationID: "installation", Name: "fixture", Platform: "WEB"},
		Session: domain.LoginSession{ID: 11, UserID: 7, DeviceID: 13,
			ExpiresAt: time.Date(2026, 12, 11, 8, 0, 0, 0, time.UTC)},
		RefreshToken: "private-refresh-token",
	}
}

func TestLoginTokenServiceSequenceAndCopies(t *testing.T) {
	ctx := context.Background()
	created := loginTokenTestSession()
	created.Device.ID, created.Device.UserID = math.MaxUint64-2, math.MaxUint64
	created.Session.ID, created.Session.UserID, created.Session.DeviceID = math.MaxUint64-1, math.MaxUint64, created.Device.ID
	expected := *created
	issued := &domain.AccessToken{Token: "private-access-token", ExpiresAt: created.Session.ExpiresAt.Add(-time.Hour)}
	expectedAccess := *issued
	var steps []string
	creator := NewLoginTokenService(func(received context.Context, account domain.VerifiedAccount, installationID, name, platform string) (*domain.LoginSessionResult, error) {
		if received != ctx || account.ID != expected.Session.UserID || installationID != "installation" || name != "请求名称" || platform != "ANDROID" {
			t.Error("会话服务未收到原始上下文、已验证账号和设备输入")
		}
		steps = append(steps, "commit")
		return created, nil
	}, func(session domain.LoginSession) (*domain.AccessToken, error) {
		steps = append(steps, "sign")
		if session != expected.Session {
			t.Error("签发器未收到完整已提交会话")
		}
		// 下游修改依赖原对象，服务返回的会话快照应保留签发时数据。
		created.Device.Name, created.Session.ID, created.RefreshToken = "随后修改", 1, "changed"
		return issued, nil
	})
	result, err := creator.Create(ctx, domain.VerifiedAccount{ID: expected.Session.UserID}, "installation", "请求名称", "ANDROID")
	if err != nil || result == nil || result.Device != expected.Device || result.Session != expected.Session || result.RefreshToken != expected.RefreshToken || result.AccessToken != expectedAccess {
		t.Fatal("凭证结果、完整 uint64 标识或副本数据不一致")
	}
	if !reflect.DeepEqual(steps, []string{"commit", "sign"}) {
		t.Fatal("签发必须发生在会话提交确认之后")
	}
	issued.Token = "changed"
	if result.AccessToken != expectedAccess {
		t.Fatal("返回结果保留了签发器的可变指针")
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), "private-") {
		t.Fatal("普通序列化泄漏了凭证原文")
	}
}

func TestLoginTokenServiceFailures(t *testing.T) {
	fault := errors.New("fixture dependency failure")
	for _, testCase := range []struct {
		stage        string
		signingCalls int
		cause        error
	}{
		{stage: "creation error", cause: fault},
		{stage: "creation error with result", cause: fault},
		{stage: "nil creation", cause: domain.ErrInvalidSessionData},
		{stage: "zero device", cause: domain.ErrInvalidSessionData},
		{stage: "wrong device owner", cause: domain.ErrInvalidSessionData},
		{stage: "wrong installation", cause: domain.ErrInvalidSessionData},
		{stage: "revoked device", cause: domain.ErrInvalidSessionData},
		{stage: "zero session", cause: domain.ErrInvalidSessionData},
		{stage: "wrong session owner", cause: domain.ErrInvalidSessionData},
		{stage: "wrong session device", cause: domain.ErrInvalidSessionData},
		{stage: "zero session expiry", cause: domain.ErrInvalidSessionData},
		{stage: "missing refresh", cause: domain.ErrInvalidSessionData},
		{stage: "signing error", signingCalls: 1, cause: fault},
		{stage: "signing error with result", signingCalls: 1, cause: fault},
		{stage: "nil token", signingCalls: 1},
		{stage: "empty token", signingCalls: 1},
		{stage: "zero token expiry", signingCalls: 1},
		{stage: "token outlives session", signingCalls: 1},
	} {
		t.Run(testCase.stage, func(t *testing.T) {
			creationCalls, signingCalls := 0, 0
			created := loginTokenTestSession()
			creator := NewLoginTokenService(func(context.Context, domain.VerifiedAccount, string, string, string) (*domain.LoginSessionResult, error) {
				creationCalls++
				switch testCase.stage {
				case "creation error":
					return nil, fault
				case "creation error with result":
					return created, fault
				case "nil creation":
					return nil, nil
				case "zero device":
					created.Device.ID = 0
				case "wrong device owner":
					created.Device.UserID = 99
				case "wrong installation":
					created.Device.InstallationID = "another-installation"
				case "revoked device":
					revoked := time.Now()
					created.Device.RevokedAt = &revoked
				case "zero session":
					created.Session.ID = 0
				case "wrong session owner":
					created.Session.UserID = 99
				case "wrong session device":
					created.Session.DeviceID = 99
				case "zero session expiry":
					created.Session.ExpiresAt = time.Time{}
				case "missing refresh":
					created.RefreshToken = ""
				}
				return created, nil
			}, func(domain.LoginSession) (*domain.AccessToken, error) {
				signingCalls++
				token := &domain.AccessToken{Token: "private-access-token", ExpiresAt: created.Session.ExpiresAt}
				switch testCase.stage {
				case "signing error":
					return nil, fault
				case "signing error with result":
					return token, fault
				case "nil token":
					return nil, nil
				case "empty token":
					token.Token = ""
				case "zero token expiry":
					token.ExpiresAt = time.Time{}
				case "token outlives session":
					token.ExpiresAt = created.Session.ExpiresAt.Add(time.Second)
				}
				return token, nil
			})
			result, err := creator.Create(context.Background(), domain.VerifiedAccount{ID: 7}, "installation", "fixture", "WEB")
			if err == nil || result != nil || creationCalls != 1 || signingCalls != testCase.signingCalls {
				t.Fatal("失败返回了部分结果、重复调用或错误地执行了签发")
			}
			if testCase.cause != nil && !errors.Is(err, testCase.cause) {
				t.Fatal("依赖错误原因未被保留")
			}
		})
	}
}

func TestLoginTokenServiceInputsAndCancellation(t *testing.T) {
	for _, stage := range []string{"nil context", "zero account", "before creation", "after commit", "after signing", "creation error and cancellation", "signing error and cancellation"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fault := errors.New("fixture failure with cancellation")
			creationCalls, signingCalls := 0, 0
			creator := NewLoginTokenService(func(context.Context, domain.VerifiedAccount, string, string, string) (*domain.LoginSessionResult, error) {
				creationCalls++
				if stage == "after commit" || stage == "creation error and cancellation" {
					cancel()
				}
				if stage == "creation error and cancellation" {
					return nil, fault
				}
				return loginTokenTestSession(), nil
			}, func(session domain.LoginSession) (*domain.AccessToken, error) {
				signingCalls++
				cancel()
				if stage == "signing error and cancellation" {
					return nil, fault
				}
				return &domain.AccessToken{Token: "private-access-token", ExpiresAt: session.ExpiresAt}, nil
			})
			account := domain.VerifiedAccount{ID: 7}
			switch stage {
			case "nil context":
				ctx = nil
			case "zero account":
				account.ID = 0
			case "before creation":
				cancel()
			}
			result, err := creator.Create(ctx, account, "installation", "fixture", "WEB")
			if err == nil || result != nil {
				t.Fatal("错误输入或取消返回了凭证结果")
			}
			if strings.Contains(stage, "error and cancellation") {
				if !errors.Is(err, fault) {
					t.Fatal("同时取消时丢失了依赖故障原因")
				}
			} else if stage != "nil context" && stage != "zero account" && !errors.Is(err, context.Canceled) {
				t.Fatal("取消原因未被保留")
			}
			if (stage == "nil context" || stage == "zero account" || stage == "before creation") && creationCalls != 0 {
				t.Fatal("错误输入或预先取消进入了会话创建")
			}
			if (stage == "after commit" || stage == "creation error and cancellation") && signingCalls != 0 {
				t.Fatal("创建失败或提交后取消仍调用了签发器")
			}
		})
	}
}

func TestLoginTokenServiceDependencies(t *testing.T) {
	for _, missingCreation := range []bool{true, false} {
		t.Run(map[bool]string{true: "creation", false: "signer"}[missingCreation], func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖应暴露装配错误")
				}
			}()
			create := func(context.Context, domain.VerifiedAccount, string, string, string) (*domain.LoginSessionResult, error) {
				return nil, nil
			}
			issue := func(domain.LoginSession) (*domain.AccessToken, error) { return nil, nil }
			if missingCreation {
				create = nil
			} else {
				issue = nil
			}
			NewLoginTokenService(create, issue)
		})
	}
}

func TestLoginTokenServiceRealComponents(t *testing.T) {
	// 1. 装配真实会话服务、随机刷新凭证生成与 JWT 签发、校验组件。
	now := time.Date(2026, 11, 11, 8, 0, 0, 123456789, time.UTC)
	var storedHash [sha256.Size]byte
	committed := false
	transaction := &loginSessionTxStub{
		ensure: func(context.Context, string, string, string) (*domain.LoginDevice, error) {
			return &domain.LoginDevice{ID: 13, UserID: 7, InstallationID: "installation"}, nil
		},
		create: func(_ context.Context, deviceID uint64, hash [sha256.Size]byte, expiresAt time.Time) (*domain.LoginSession, error) {
			storedHash = hash
			return &domain.LoginSession{ID: 11, UserID: 7, DeviceID: deviceID, ExpiresAt: expiresAt}, nil
		},
	}
	cfg := accessTokenTestConfig()
	// 让会话先于默认 15 分钟访问令牌到期，验证组合流程沿用会话期限上限。
	cfg.RefreshTokenTTL = 2 * time.Minute
	sessions, err := NewLoginSessionService(cfg, func(ctx context.Context, _ uint64, action func(context.Context, LoginSessionTransaction) error) error {
		if err := action(ctx, transaction); err != nil {
			return err
		}
		committed = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sessions.now = func() time.Time { return now }
	signer, err := NewAccessTokenSigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	signer.now = func() time.Time { return now }
	verifier, err := NewAccessTokenVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	verifier.now = func() time.Time { return now }
	creator := NewLoginTokenService(sessions.Create, func(session domain.LoginSession) (*domain.AccessToken, error) {
		if !committed {
			t.Error("事务确认提交前调用了 JWT 签发器")
		}
		return signer.Issue(session)
	})

	// 2. 比对真实刷新摘要，并用校验器核对 JWT 用户、会话及期限。
	result, err := creator.Create(context.Background(), domain.VerifiedAccount{ID: 7}, "installation", "fixture", "WEB")
	if err != nil || result == nil || sha256.Sum256([]byte(result.RefreshToken)) != storedHash {
		t.Fatal("真实组件组合失败或刷新凭证摘要不一致")
	}
	identity, err := verifier.Verify(result.AccessToken.Token)
	if err != nil || identity == nil || identity.UserID != result.Session.UserID || identity.SessionID != result.Session.ID ||
		!result.AccessToken.ExpiresAt.Equal(result.Session.ExpiresAt.Truncate(time.Second)) {
		t.Fatal("签发的 JWT 与已提交会话或期限不一致")
	}
}

func TestLoginTokenServiceExpiredCommittedSession(t *testing.T) {
	created := loginTokenTestSession()
	signer, err := NewAccessTokenSigner(accessTokenTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	signer.now = func() time.Time { return created.Session.ExpiresAt }
	creator := NewLoginTokenService(func(context.Context, domain.VerifiedAccount, string, string, string) (*domain.LoginSessionResult, error) {
		return created, nil
	}, signer.Issue)
	result, err := creator.Create(context.Background(), domain.VerifiedAccount{ID: 7}, "installation", "fixture", "WEB")
	if result != nil || !errors.Is(err, domain.ErrSessionExpired) {
		t.Fatal("提交后会话到期未由真实签发器拒绝")
	}
}

func TestLoginTokenServiceDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	creator := NewLoginTokenService(func(ctx context.Context, _ domain.VerifiedAccount, _ string, _ string, _ string) (*domain.LoginSessionResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, func(domain.LoginSession) (*domain.AccessToken, error) {
		t.Error("会话创建超时仍调用了签发器")
		return nil, nil
	})
	result, err := creator.Create(ctx, domain.VerifiedAccount{ID: 7}, "installation", "fixture", "WEB")
	if result != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("会话创建超时未保留对应原因")
	}
}
