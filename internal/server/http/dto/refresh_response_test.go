package dto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func refreshDataFixture() (*domain.RefreshTokenResult, time.Time) {
	now := time.Date(2026, 11, 12, 8, 0, 0, 250000000, time.UTC)
	return &domain.RefreshTokenResult{
		Session:         domain.LoginSession{ID: 11, UserID: 7, DeviceID: 13, ExpiresAt: now.Add(30 * 24 * time.Hour).In(time.FixedZone("UTC+8", 8*60*60))},
		TokenGeneration: 2,
		AccessToken:     domain.AccessToken{Token: "fixture-access-token", ExpiresAt: now.Truncate(time.Second).Add(15 * time.Minute)},
		RefreshToken:    "fixture-refresh-token",
	}, now
}

func TestNewRefreshDataJSON(t *testing.T) {
	for _, clientType := range []string{"web", "native"} {
		t.Run(clientType, func(t *testing.T) {
			result, now := refreshDataFixture()
			before := *result
			data, err := NewRefreshData(result, clientType, now)
			if err != nil || data == nil || data.ExpiresIn != 899 || *result != before {
				t.Fatal("转换结果、秒数或原对象错误", err)
			}
			response := RefreshResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatal("刷新响应 JSON 编码失败", err)
			}
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.UseNumber()
			var body map[string]any
			if err := decoder.Decode(&body); err != nil || len(body) != 2 || body["request_id"] != response.RequestID {
				t.Fatal("外层响应字段错误", err)
			}
			object, valid := body["data"].(map[string]any)
			if !valid || object["access_token"] != result.AccessToken.Token || object["expires_in"] != json.Number("899") {
				t.Fatal("刷新数据字段错误")
			}
			expectedFields := 2
			if clientType == "native" {
				expectedFields++
				if object["refresh_token"] != result.RefreshToken {
					t.Fatal("原生响应未交付刷新原文")
				}
			} else if _, exists := object["refresh_token"]; exists || strings.Contains(string(encoded), result.RefreshToken) {
				t.Fatal("Web JSON 包含刷新原文")
			}
			if len(object) != expectedFields {
				t.Fatal("响应包含非约定字段")
			}
			data.AccessToken, data.RefreshToken, data.ExpiresIn = "changed", "changed", 0
			if *result != before {
				t.Fatal("修改 DTO 影响了内部结果")
			}
		})
	}
}

func TestNewRefreshDataBoundaries(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		change  func(*domain.RefreshTokenResult, *time.Time)
		seconds int64
	}{
		{name: "exact seconds", change: func(result *domain.RefreshTokenResult, now *time.Time) {
			result.AccessToken.ExpiresAt = now.Add(10 * time.Second)
		}, seconds: 10},
		{name: "fractional remaining", change: func(result *domain.RefreshTokenResult, now *time.Time) {
			result.AccessToken.ExpiresAt = now.Add(1500 * time.Millisecond)
		}, seconds: 1},
		{name: "future subsecond remains explicit zero", change: func(result *domain.RefreshTokenResult, now *time.Time) {
			result.AccessToken.ExpiresAt = now.Add(time.Nanosecond)
		}},
		{name: "same instant in another zone", change: func(_ *domain.RefreshTokenResult, now *time.Time) { *now = now.In(time.FixedZone("UTC-5", -5*60*60)) }, seconds: 899},
		{name: "maximum internal identifiers and generation", change: func(result *domain.RefreshTokenResult, _ *time.Time) {
			result.Session.ID, result.Session.UserID, result.Session.DeviceID, result.TokenGeneration = ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)
		}, seconds: 899},
		{name: "microsecond session precision", change: func(result *domain.RefreshTokenResult, now *time.Time) {
			result.Session.ExpiresAt = now.Add(time.Hour).Add(789 * time.Nanosecond)
		}, seconds: 899},
		{name: "access cutoff equals session cutoff", change: func(result *domain.RefreshTokenResult, now *time.Time) {
			result.Session.ExpiresAt = now.Add(10 * time.Second)
			result.AccessToken.ExpiresAt = result.Session.ExpiresAt
		}, seconds: 10},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result, now := refreshDataFixture()
			testCase.change(result, &now)
			data, err := NewRefreshData(result, "native", now)
			if err != nil || data == nil || data.ExpiresIn != testCase.seconds {
				t.Fatal("边界转换错误", err)
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &object); err != nil {
				t.Fatal(err)
			}
			var seconds int64
			value, exists := object["expires_in"]
			if !exists || json.Unmarshal(value, &seconds) != nil || seconds != testCase.seconds {
				t.Fatal("剩余秒数或显式零值 JSON 错误")
			}
		})
	}
}

func TestNewRefreshDataFailures(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		change func(*domain.RefreshTokenResult, *time.Time)
		want   error
	}{
		{name: "zero user", change: func(result *domain.RefreshTokenResult, _ *time.Time) { result.Session.UserID = 0 }, want: domain.ErrInvalidSessionData},
		{name: "zero session", change: func(result *domain.RefreshTokenResult, _ *time.Time) { result.Session.ID = 0 }, want: domain.ErrInvalidSessionData},
		{name: "zero device", change: func(result *domain.RefreshTokenResult, _ *time.Time) { result.Session.DeviceID = 0 }, want: domain.ErrInvalidSessionData},
		{name: "zero generation", change: func(result *domain.RefreshTokenResult, _ *time.Time) { result.TokenGeneration = 0 }, want: domain.ErrInvalidSessionData},
		{name: "unrotated generation", change: func(result *domain.RefreshTokenResult, _ *time.Time) { result.TokenGeneration = 1 }, want: domain.ErrInvalidSessionData},
		{name: "empty access", change: func(result *domain.RefreshTokenResult, _ *time.Time) { result.AccessToken.Token = "" }},
		{name: "empty refresh", change: func(result *domain.RefreshTokenResult, _ *time.Time) { result.RefreshToken = "" }},
		{name: "zero response time", change: func(_ *domain.RefreshTokenResult, now *time.Time) { *now = time.Time{} }},
		{name: "zero session expiry", change: func(result *domain.RefreshTokenResult, _ *time.Time) { result.Session.ExpiresAt = time.Time{} }, want: domain.ErrInvalidSessionData},
		{name: "session year too small", change: func(result *domain.RefreshTokenResult, _ *time.Time) {
			result.Session.ExpiresAt = time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC)
		}, want: domain.ErrInvalidSessionData},
		{name: "session year too large", change: func(result *domain.RefreshTokenResult, _ *time.Time) {
			result.Session.ExpiresAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		}, want: domain.ErrInvalidSessionData},
		{name: "zero access expiry", change: func(result *domain.RefreshTokenResult, _ *time.Time) { result.AccessToken.ExpiresAt = time.Time{} }, want: domain.ErrInvalidSessionData},
		{name: "access outlives session", change: func(result *domain.RefreshTokenResult, _ *time.Time) {
			result.AccessToken.ExpiresAt = result.Session.ExpiresAt.Add(time.Second)
		}, want: domain.ErrInvalidSessionData},
		{name: "access outlives normalized session", change: func(result *domain.RefreshTokenResult, now *time.Time) {
			result.Session.ExpiresAt = now.Add(time.Hour).Add(789 * time.Nanosecond)
			result.AccessToken.ExpiresAt = result.Session.ExpiresAt
		}, want: domain.ErrInvalidSessionData},
		{name: "session expired at boundary", change: func(result *domain.RefreshTokenResult, now *time.Time) {
			result.Session.ExpiresAt, result.AccessToken.ExpiresAt = *now, now.Add(-time.Second)
		}, want: domain.ErrSessionExpired},
		{name: "session expired before now", change: func(result *domain.RefreshTokenResult, now *time.Time) {
			result.Session.ExpiresAt, result.AccessToken.ExpiresAt = now.Add(-time.Second), now.Add(-2*time.Second)
		}, want: domain.ErrSessionExpired},
		{name: "access expired at boundary", change: func(result *domain.RefreshTokenResult, now *time.Time) { result.AccessToken.ExpiresAt = *now }, want: domain.ErrAccessTokenExpired},
		{name: "access expired before now", change: func(result *domain.RefreshTokenResult, now *time.Time) {
			result.AccessToken.ExpiresAt = now.Add(-time.Second)
		}, want: domain.ErrAccessTokenExpired},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			for _, clientType := range []string{"web", "native"} {
				result, now := refreshDataFixture()
				testCase.change(result, &now)
				data, err := NewRefreshData(result, clientType, now)
				if data != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) {
					t.Fatal("错误输入未拒绝或原因丢失", err)
				}
				if strings.Contains(err.Error(), "fixture-access-token") || strings.Contains(err.Error(), "fixture-refresh-token") {
					t.Fatal("错误提示包含凭证原文")
				}
			}
		})
	}
	for _, input := range []string{"nil result", "empty client", "unknown client", "uppercase client", "padded client"} {
		t.Run(input, func(t *testing.T) {
			result, now := refreshDataFixture()
			clientType := "native"
			switch input {
			case "nil result":
				result = nil
			case "empty client":
				clientType = ""
			case "unknown client":
				clientType = "other"
			case "uppercase client":
				clientType = "WEB"
			case "padded client":
				clientType = " web "
			}
			if data, err := NewRefreshData(result, clientType, now); data != nil || err == nil {
				t.Fatal("无效客户端或空结果未拒绝")
			}
		})
	}
}

func TestNewRefreshDataRealJWT(t *testing.T) {
	configuration := config.JWTConfig{SigningKey: bytes.Repeat([]byte{7}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test", AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour}
	signer, err := service.NewAccessTokenSigner(configuration)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewAccessTokenVerifier(configuration)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := service.GenerateRefreshToken()
	session := domain.LoginSession{ID: 11, UserID: 7, DeviceID: 13, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
	refresher := service.NewRefreshTokenService(func(context.Context, string) (*domain.RefreshSessionResult, error) {
		return &domain.RefreshSessionResult{Session: session, TokenGeneration: 2, RefreshToken: raw}, nil
	}, signer.Issue)
	result, err := refresher.Refresh(context.Background(), "old-input")
	if err != nil || result == nil {
		t.Fatal("真实签发组合失败", err)
	}
	for _, clientType := range []string{"web", "native"} {
		data, err := NewRefreshData(result, clientType, time.Now())
		if err != nil || data == nil {
			t.Fatal("真实刷新结果转换失败", err)
		}
		identity, err := verifier.Verify(data.AccessToken)
		if err != nil || identity == nil || identity.UserID != session.UserID || identity.SessionID != session.ID {
			t.Fatal("DTO 中的 JWT 校验失败", err)
		}
		if clientType == "native" && data.RefreshToken != raw {
			t.Fatal("原生原文与服务结果不一致")
		}
	}
}
