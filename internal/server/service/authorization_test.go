package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestAuthorizationServiceOutcomesAndOrder(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 123456000, time.UTC)
	queryFailure := errors.New("fixture query fault")
	for _, testCase := range []struct {
		name                   string
		prepare                func(*domain.VerifiedAccessToken, *domain.SessionAuthorization)
		tokenError, queryError error
		nilIdentity, nilState  bool
		wantError              error
		wantSequence           string
	}{
		{name: "active", wantSequence: "verify,query"},
		{name: "invalid token", tokenError: domain.ErrInvalidAccessToken, wantError: domain.ErrInvalidAccessToken, wantSequence: "verify"},
		{name: "expired token rejected by verifier", tokenError: domain.ErrAccessTokenExpired, wantError: domain.ErrAccessTokenExpired, wantSequence: "verify"},
		{name: "nil identity", nilIdentity: true, wantError: domain.ErrInvalidSessionData, wantSequence: "verify"},
		{name: "zero user", prepare: func(identity *domain.VerifiedAccessToken, _ *domain.SessionAuthorization) { identity.UserID = 0 }, wantError: domain.ErrInvalidSessionData, wantSequence: "verify"},
		{name: "zero session", prepare: func(identity *domain.VerifiedAccessToken, _ *domain.SessionAuthorization) { identity.SessionID = 0 }, wantError: domain.ErrInvalidSessionData, wantSequence: "verify"},
		{name: "zero token expiry", prepare: func(identity *domain.VerifiedAccessToken, _ *domain.SessionAuthorization) {
			identity.ExpiresAt = time.Time{}
		}, wantError: domain.ErrInvalidSessionData, wantSequence: "verify"},
		{name: "missing session", queryError: domain.ErrSessionNotFound, wantError: domain.ErrSessionNotFound, wantSequence: "verify,query"},
		{name: "wrapped missing", queryError: fmt.Errorf("fixture wrapper: %w", domain.ErrSessionNotFound), wantError: domain.ErrSessionNotFound, wantSequence: "verify,query"},
		{name: "query fault", queryError: queryFailure, wantError: queryFailure, wantSequence: "verify,query"},
		{name: "nil state", nilState: true, wantError: domain.ErrInvalidSessionData, wantSequence: "verify,query"},
		{name: "wrong owner", prepare: func(_ *domain.VerifiedAccessToken, state *domain.SessionAuthorization) { state.UserID = 8 }, wantError: domain.ErrInvalidSessionData, wantSequence: "verify,query"},
		{name: "wrong session", prepare: func(_ *domain.VerifiedAccessToken, state *domain.SessionAuthorization) { state.SessionID = 12 }, wantError: domain.ErrInvalidSessionData, wantSequence: "verify,query"},
		{name: "zero device", prepare: func(_ *domain.VerifiedAccessToken, state *domain.SessionAuthorization) { state.DeviceID = 0 }, wantError: domain.ErrInvalidSessionData, wantSequence: "verify,query"},
		{name: "zero session expiry", prepare: func(_ *domain.VerifiedAccessToken, state *domain.SessionAuthorization) {
			state.SessionExpiresAt = time.Time{}
		}, wantError: domain.ErrInvalidSessionData, wantSequence: "verify,query"},
		{name: "disabled", prepare: func(_ *domain.VerifiedAccessToken, state *domain.SessionAuthorization) { state.UserStatus = "DISABLED" }, wantError: domain.ErrAccountDisabled, wantSequence: "verify,query"},
		{name: "unknown status", prepare: func(_ *domain.VerifiedAccessToken, state *domain.SessionAuthorization) { state.UserStatus = "UNKNOWN" }, wantError: domain.ErrInvalidAccountData, wantSequence: "verify,query"},
		{name: "revoked session", prepare: func(_ *domain.VerifiedAccessToken, state *domain.SessionAuthorization) { state.SessionRevokedAt = &now }, wantError: domain.ErrSessionRevoked, wantSequence: "verify,query"},
		{name: "revoked device", prepare: func(_ *domain.VerifiedAccessToken, state *domain.SessionAuthorization) { state.DeviceRevokedAt = &now }, wantError: domain.ErrDeviceRevoked, wantSequence: "verify,query"},
		{name: "token elapsed during request", prepare: func(identity *domain.VerifiedAccessToken, _ *domain.SessionAuthorization) {
			identity.ExpiresAt = now.Add(-time.Nanosecond)
		}, wantError: domain.ErrAccessTokenExpired, wantSequence: "verify,query"},
		{name: "token exact boundary", prepare: func(identity *domain.VerifiedAccessToken, _ *domain.SessionAuthorization) { identity.ExpiresAt = now }, wantError: domain.ErrAccessTokenExpired, wantSequence: "verify,query"},
		{name: "session expired", prepare: func(_ *domain.VerifiedAccessToken, state *domain.SessionAuthorization) {
			state.SessionExpiresAt = now.Add(-time.Nanosecond)
		}, wantError: domain.ErrSessionExpired, wantSequence: "verify,query"},
		{name: "session exact boundary", prepare: func(_ *domain.VerifiedAccessToken, state *domain.SessionAuthorization) { state.SessionExpiresAt = now }, wantError: domain.ErrSessionExpired, wantSequence: "verify,query"},
		{name: "disabled precedence", prepare: func(_ *domain.VerifiedAccessToken, state *domain.SessionAuthorization) {
			state.UserStatus = "DISABLED"
			state.SessionRevokedAt = &now
			state.DeviceRevokedAt = &now
		}, wantError: domain.ErrAccountDisabled, wantSequence: "verify,query"},
		{name: "session revocation precedence", prepare: func(_ *domain.VerifiedAccessToken, state *domain.SessionAuthorization) {
			state.SessionRevokedAt = &now
			state.DeviceRevokedAt = &now
		}, wantError: domain.ErrSessionRevoked, wantSequence: "verify,query"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			identity := domain.VerifiedAccessToken{UserID: 7, SessionID: 11, ExpiresAt: now.Add(time.Minute)}
			state := domain.SessionAuthorization{UserID: 7, SessionID: 11, DeviceID: 13, UserStatus: "ACTIVE", SessionExpiresAt: now.Add(time.Hour)}
			if testCase.prepare != nil {
				testCase.prepare(&identity, &state)
			}
			beforeIdentity, beforeState := identity, state
			ctx := context.Background()
			var sequence []string
			service := NewAuthorizationService(func(tokenText string) (*domain.VerifiedAccessToken, error) {
				sequence = append(sequence, "verify")
				if tokenText != "private-token-fixture" {
					t.Error("令牌原文没有原样传递")
				}
				if testCase.nilIdentity {
					return nil, testCase.tokenError
				}
				return &identity, testCase.tokenError
			}, func(received context.Context, userID, sessionID uint64) (*domain.SessionAuthorization, error) {
				sequence = append(sequence, "query")
				if received != ctx || userID != 7 || sessionID != 11 {
					t.Error("上下文或已验证身份没有传给查询")
				}
				if testCase.nilState {
					return nil, testCase.queryError
				}
				return &state, testCase.queryError
			})
			service.now = func() time.Time { return now }
			actor, err := service.Authorize(ctx, "private-token-fixture")
			if testCase.wantError == nil {
				if err != nil || actor == nil || *actor != (domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}) {
					t.Fatalf("授权成功结果错误: %v", err)
				}
			} else if actor != nil || !errors.Is(err, testCase.wantError) {
				t.Fatalf("失败返回了身份或丢失错误原因: %v", err)
			}
			if strings.Join(sequence, ",") != testCase.wantSequence || identity != beforeIdentity || state != beforeState {
				t.Error("调用顺序不符或服务修改了提供方的数据")
			}
			if err != nil && strings.Contains(err.Error(), "private-token") {
				t.Error("错误带出了令牌原文")
			}
		})
	}
}

func TestAuthorizationServiceQueryDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	service := NewAuthorizationService(func(string) (*domain.VerifiedAccessToken, error) {
		return &domain.VerifiedAccessToken{UserID: 7, SessionID: 11, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}, func(received context.Context, _, _ uint64) (*domain.SessionAuthorization, error) {
		if received != ctx {
			return nil, errors.New("查询上下文没有传递")
		}
		<-received.Done()
		return nil, received.Err()
	})
	actor, err := service.Authorize(ctx, "fixture")
	if actor != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("授权查询到期后返回了身份或丢失原因")
	}
}

func TestAuthorizationServiceConstructionAndCancellation(t *testing.T) {
	verify := func(string) (*domain.VerifiedAccessToken, error) { return nil, domain.ErrInvalidAccessToken }
	query := func(context.Context, uint64, uint64) (*domain.SessionAuthorization, error) {
		return nil, domain.ErrSessionNotFound
	}
	for _, missing := range []string{"verify", "query"} {
		t.Run("missing "+missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖仍构造成功")
				}
			}()
			if missing == "verify" {
				NewAuthorizationService(nil, query)
			} else {
				NewAuthorizationService(verify, nil)
			}
		})
	}
	for _, phase := range []string{"nil context", "before verify", "after verify", "after query", "before return"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "nil context" {
				ctx = nil
			} else if phase == "before verify" {
				cancel()
			}
			var sequence []string
			service := NewAuthorizationService(func(string) (*domain.VerifiedAccessToken, error) {
				sequence = append(sequence, "verify")
				if phase == "after verify" {
					cancel()
				}
				return &domain.VerifiedAccessToken{UserID: 7, SessionID: 11, ExpiresAt: time.Now().Add(time.Hour)}, nil
			}, func(context.Context, uint64, uint64) (*domain.SessionAuthorization, error) {
				sequence = append(sequence, "query")
				if phase == "after query" {
					cancel()
				}
				return &domain.SessionAuthorization{UserID: 7, SessionID: 11, DeviceID: 13, UserStatus: "ACTIVE", SessionExpiresAt: time.Now().Add(time.Hour)}, nil
			})
			service.now = func() time.Time {
				if phase == "before return" {
					cancel()
				}
				return time.Now()
			}
			actor, err := service.Authorize(ctx, "fixture")
			if actor != nil || err == nil || (phase != "nil context" && !errors.Is(err, context.Canceled)) {
				t.Error("取消或无效上下文仍返回身份")
			}
			want := "verify,query"
			if phase == "nil context" || phase == "before verify" {
				want = ""
			} else if phase == "after verify" {
				want = "verify"
			}
			if strings.Join(sequence, ",") != want {
				t.Error("取消后继续调用了依赖")
			}
		})
	}
}

func TestAuthorizationServiceWithRealTokens(t *testing.T) {
	jwtConfig := accessTokenTestConfig()
	signer, err := NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewAccessTokenVerifier(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	signer.now = func() time.Time { return now }
	verifier.now = func() time.Time { return now }
	session := domain.LoginSession{ID: 11, UserID: 7, DeviceID: 13, ExpiresAt: now.Add(time.Hour)}
	issued, err := signer.Issue(session)
	if err != nil {
		t.Fatal(err)
	}
	state := domain.SessionAuthorization{UserID: 7, SessionID: 11, DeviceID: 13, UserStatus: "ACTIVE", SessionExpiresAt: session.ExpiresAt}
	queryCount := 0
	advanceDuringQuery := false
	service := NewAuthorizationService(verifier.Verify, func(_ context.Context, userID, sessionID uint64) (*domain.SessionAuthorization, error) {
		queryCount++
		if userID != 7 || sessionID != 11 {
			t.Error("真实令牌身份解析错误")
		}
		if advanceDuringQuery {
			now = issued.ExpiresAt
		}
		return &state, nil
	})
	service.now = func() time.Time { return now }
	if actor, err := service.Authorize(context.Background(), issued.Token); err != nil || actor == nil || actor.DeviceID != 13 {
		t.Fatal("真实签发/校验与授权闭环失败")
	}
	state.SessionRevokedAt = &now
	if actor, err := service.Authorize(context.Background(), issued.Token); actor != nil || err != domain.ErrSessionRevoked {
		t.Fatal("状态变化后旧令牌仍获授权")
	}
	state.SessionRevokedAt = nil
	advanceDuringQuery = true
	if actor, err := service.Authorize(context.Background(), issued.Token); actor != nil || err != domain.ErrAccessTokenExpired {
		t.Fatal("查询期间到期的真实令牌仍获授权")
	}
	before := queryCount
	if actor, err := service.Authorize(context.Background(), "private-invalid-token"); actor != nil || err != domain.ErrInvalidAccessToken || queryCount != before {
		t.Fatal("无效令牌仍查询了数据库状态")
	}
}

func TestAuthorizationServiceConcurrent(t *testing.T) {
	jwtConfig := accessTokenTestConfig()
	signer, err := NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewAccessTokenVerifier(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	signer.now = func() time.Time { return now }
	verifier.now = func() time.Time { return now }
	service := NewAuthorizationService(verifier.Verify, func(_ context.Context, userID, sessionID uint64) (*domain.SessionAuthorization, error) {
		state := &domain.SessionAuthorization{UserID: userID, SessionID: sessionID, DeviceID: sessionID + 100, UserStatus: "ACTIVE", SessionExpiresAt: now.Add(time.Hour)}
		if sessionID%4 == 0 {
			state.SessionRevokedAt = &now
		}
		return state, nil
	})
	service.now = func() time.Time { return now }
	type outcome struct {
		identifier uint64
		actor      *domain.Actor
		err        error
	}
	results := make(chan outcome, 32)
	var workers sync.WaitGroup
	for index := range cap(results) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			identifier := uint64(index + 1)
			issued, err := signer.Issue(domain.LoginSession{ID: identifier, UserID: identifier, DeviceID: identifier + 100, ExpiresAt: now.Add(time.Hour)})
			if err != nil {
				results <- outcome{identifier: identifier, err: err}
				return
			}
			actor, err := service.Authorize(context.Background(), issued.Token)
			results <- outcome{identifier: identifier, actor: actor, err: err}
		}()
	}
	workers.Wait()
	close(results)
	for result := range results {
		if result.identifier%4 == 0 {
			if result.actor != nil || result.err != domain.ErrSessionRevoked {
				t.Error("并发撤销状态未拒绝请求")
			}
		} else if result.err != nil || result.actor == nil || result.actor.UserID != result.identifier || result.actor.SessionID != result.identifier || result.actor.DeviceID != result.identifier+100 {
			t.Error("并发授权的身份不符")
		}
	}
}
