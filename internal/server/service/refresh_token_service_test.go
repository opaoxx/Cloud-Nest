package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func refreshTokenTestSession() *domain.RefreshSessionResult {
	raw, verified := refreshSessionFixture()
	return &domain.RefreshSessionResult{Session: verified.Session, TokenGeneration: 2, RefreshToken: raw}
}

func TestRefreshTokenServiceSequenceAndCopies(t *testing.T) {
	ctx := context.Background()
	refreshed := refreshTokenTestSession()
	refreshed.Session.ID, refreshed.Session.UserID, refreshed.Session.DeviceID = ^uint64(0), ^uint64(0), ^uint64(0)
	refreshed.TokenGeneration = ^uint64(0)
	expected := *refreshed
	issued := &domain.AccessToken{Token: "private-access-token", ExpiresAt: expected.Session.ExpiresAt.Add(-time.Minute)}
	expectedAccess := *issued
	var steps []string
	refresher := NewRefreshTokenService(func(received context.Context, raw string) (*domain.RefreshSessionResult, error) {
		if received != ctx || raw != " original raw " {
			t.Error("刷新会话未收到原始上下文和原文")
		}
		steps = append(steps, "commit")
		return refreshed, nil
	}, func(session domain.LoginSession) (*domain.AccessToken, error) {
		steps = append(steps, "sign")
		if session != expected.Session {
			t.Error("签发未使用完整已提交会话")
		}
		refreshed.Session.ID, refreshed.TokenGeneration, refreshed.RefreshToken = 1, 1, "changed"
		return issued, nil
	})
	result, err := refresher.Refresh(ctx, " original raw ")
	if err != nil || result == nil || result.Session != expected.Session || result.TokenGeneration != expected.TokenGeneration ||
		result.RefreshToken != expected.RefreshToken || result.AccessToken != expectedAccess {
		t.Fatal("组合结果或值副本错误", err)
	}
	if !reflect.DeepEqual(steps, []string{"commit", "sign"}) {
		t.Fatal("签发发生在确认提交之前", steps)
	}
	issued.Token = "changed"
	if result.AccessToken != expectedAccess {
		t.Fatal("结果持有签发对象的可变指针")
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), expected.RefreshToken) || strings.Contains(string(encoded), expectedAccess.Token) || strings.Contains(string(encoded), "RefreshToken") {
		t.Fatal("普通 JSON 包含凭证原文")
	}
}

func TestRefreshTokenServiceFailures(t *testing.T) {
	fault := errors.New("fixture dependency failure")
	for _, testCase := range []struct {
		name         string
		want         error
		signingCalls int
	}{
		{name: "session error", want: fault},
		{name: "session error with result", want: fault},
		{name: "invalid old credential", want: domain.ErrInvalidRefreshToken},
		{name: "nil session", want: domain.ErrInvalidSessionData},
		{name: "zero user", want: domain.ErrInvalidSessionData},
		{name: "zero session", want: domain.ErrInvalidSessionData},
		{name: "zero device", want: domain.ErrInvalidSessionData},
		{name: "zero session expiry", want: domain.ErrInvalidSessionData},
		{name: "zero generation", want: domain.ErrInvalidSessionData},
		{name: "unrotated generation", want: domain.ErrInvalidSessionData},
		{name: "empty refresh", want: domain.ErrInvalidSessionData},
		{name: "sign error", want: fault, signingCalls: 1},
		{name: "sign error with result", want: fault, signingCalls: 1},
		{name: "session expired at signing", want: domain.ErrSessionExpired, signingCalls: 1},
		{name: "nil access", signingCalls: 1},
		{name: "empty access", signingCalls: 1},
		{name: "zero access expiry", signingCalls: 1},
		{name: "access outlives session", signingCalls: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			refreshed := refreshTokenTestSession()
			refreshCalls, signingCalls := 0, 0
			refresher := NewRefreshTokenService(func(context.Context, string) (*domain.RefreshSessionResult, error) {
				refreshCalls++
				switch testCase.name {
				case "session error", "invalid old credential":
					return nil, testCase.want
				case "session error with result":
					return refreshed, testCase.want
				case "nil session":
					return nil, nil
				case "zero user":
					refreshed.Session.UserID = 0
				case "zero session":
					refreshed.Session.ID = 0
				case "zero device":
					refreshed.Session.DeviceID = 0
				case "zero session expiry":
					refreshed.Session.ExpiresAt = time.Time{}
				case "zero generation":
					refreshed.TokenGeneration = 0
				case "unrotated generation":
					refreshed.TokenGeneration = 1
				case "empty refresh":
					refreshed.RefreshToken = ""
				}
				return refreshed, nil
			}, func(session domain.LoginSession) (*domain.AccessToken, error) {
				signingCalls++
				issued := &domain.AccessToken{Token: "private-access", ExpiresAt: session.ExpiresAt.Add(-time.Minute)}
				switch testCase.name {
				case "sign error", "session expired at signing":
					return nil, testCase.want
				case "sign error with result":
					return issued, testCase.want
				case "nil access":
					return nil, nil
				case "empty access":
					issued.Token = ""
				case "zero access expiry":
					issued.ExpiresAt = time.Time{}
				case "access outlives session":
					issued.ExpiresAt = session.ExpiresAt.Add(time.Second)
				}
				return issued, nil
			})
			result, err := refresher.Refresh(context.Background(), "private-old-refresh")
			if result != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) ||
				refreshCalls != 1 || signingCalls != testCase.signingCalls {
				t.Fatal("错误结果、调用次数或错误链异常", err)
			}
			if strings.Contains(err.Error(), "private-") {
				t.Error("错误提示包含凭证原文")
			}
		})
	}
}

func TestRefreshTokenServiceContexts(t *testing.T) {
	for _, stage := range []string{"nil", "before refresh", "refresh deadline", "after refresh", "after sign", "refresh error and cancellation", "sign error and cancellation"} {
		t.Run(stage, func(t *testing.T) {
			deadline := time.Second
			if stage == "refresh deadline" {
				deadline = 20 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			defer cancel()
			fault := errors.New("fixture original failure")
			refreshCalls, signingCalls := 0, 0
			refreshed := refreshTokenTestSession()
			refresher := NewRefreshTokenService(func(context.Context, string) (*domain.RefreshSessionResult, error) {
				refreshCalls++
				if stage == "refresh deadline" {
					<-ctx.Done()
				}
				if stage == "after refresh" || stage == "refresh error and cancellation" {
					cancel()
				}
				if stage == "refresh error and cancellation" {
					return refreshed, fault
				}
				return refreshed, nil
			}, func(session domain.LoginSession) (*domain.AccessToken, error) {
				signingCalls++
				cancel()
				if stage == "sign error and cancellation" {
					return nil, fault
				}
				return &domain.AccessToken{Token: "private-access", ExpiresAt: session.ExpiresAt.Add(-time.Minute)}, nil
			})
			var callContext context.Context = ctx
			if stage == "nil" {
				callContext = nil
			}
			if stage == "before refresh" {
				cancel()
			}
			result, err := refresher.Refresh(callContext, "original")
			want := error(context.Canceled)
			if stage == "refresh deadline" {
				want = context.DeadlineExceeded
			}
			if strings.Contains(stage, "error and cancellation") {
				want = fault
			}
			if result != nil || err == nil || (stage != "nil" && !errors.Is(err, want)) {
				t.Fatal("取消或依赖故障的优先级异常", err)
			}
			if (stage == "nil" || stage == "before refresh") && refreshCalls != 0 {
				t.Error("无效上下文进入刷新")
			}
			wantSigning := 0
			if stage == "after sign" || stage == "sign error and cancellation" {
				wantSigning = 1
			}
			if signingCalls != wantSigning {
				t.Error("签发调用次数不符合取消边界")
			}
		})
	}
}

func TestRefreshTokenServiceRealJWT(t *testing.T) {
	refreshed := refreshTokenTestSession()
	refreshed.Session.ExpiresAt = time.Now().UTC().Add(2 * time.Minute).Truncate(time.Second)
	configuration := accessTokenTestConfig()
	signer, err := NewAccessTokenSigner(configuration)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewAccessTokenVerifier(configuration)
	if err != nil {
		t.Fatal(err)
	}
	refresher := NewRefreshTokenService(func(context.Context, string) (*domain.RefreshSessionResult, error) { return refreshed, nil }, signer.Issue)
	result, err := refresher.Refresh(context.Background(), "original")
	if err != nil || result == nil {
		t.Fatal("真实签发失败", err)
	}
	identity, err := verifier.Verify(result.AccessToken.Token)
	if err != nil || identity == nil || identity.UserID != result.Session.UserID || identity.SessionID != result.Session.ID ||
		!identity.ExpiresAt.Equal(result.AccessToken.ExpiresAt) || !result.AccessToken.ExpiresAt.Equal(result.Session.ExpiresAt) {
		t.Fatal("真实 JWT 与提交会话不一致", err)
	}
}

func TestRefreshTokenServiceDependencies(t *testing.T) {
	for _, missing := range []string{"session", "signer"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖仍构造成功")
				}
			}()
			refresh := func(context.Context, string) (*domain.RefreshSessionResult, error) { return nil, nil }
			issue := func(domain.LoginSession) (*domain.AccessToken, error) { return nil, nil }
			if missing == "session" {
				refresh = nil
			} else {
				issue = nil
			}
			NewRefreshTokenService(refresh, issue)
		})
	}
}
