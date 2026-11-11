package dto

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func loginDataFixture() (*domain.LoginTokenResult, *domain.UserProfile, time.Time) {
	now := time.Date(2026, 11, 12, 8, 0, 0, 250000000, time.UTC)
	profile := &domain.UserProfile{ID: 7, Username: "fixture", DisplayName: " 学习用户 🌱 ", Status: "ACTIVE", RootEntryID: 17, QuotaBytes: 100, UsedBytes: 60, ReservedBytes: 40}
	result := &domain.LoginTokenResult{
		Device: domain.LoginDevice{ID: 13, UserID: 7, Name: " 用户改过的设备名 ", Platform: "WINDOWS"},
		Session: domain.LoginSession{ID: 11, UserID: 7, DeviceID: 13,
			ExpiresAt: now.Add(30 * 24 * time.Hour).In(time.FixedZone("UTC+8", 8*60*60))},
		AccessToken:  domain.AccessToken{Token: "fixture-access-token", ExpiresAt: now.Truncate(time.Second).Add(15 * time.Minute)},
		RefreshToken: "fixture-refresh-token",
	}
	return result, profile, now
}

func TestNewLoginDataJSON(t *testing.T) {
	for _, clientType := range []string{"web", "native"} {
		t.Run(clientType, func(t *testing.T) {
			result, profile, now := loginDataFixture()
			beforeResult, beforeProfile := *result, *profile
			data, err := NewLoginData(result, profile, clientType, now)
			if err != nil || data == nil || data.ExpiresIn != 899 || data.Session.ExpiresAt.Location() != time.UTC || *result != beforeResult || *profile != beforeProfile {
				t.Fatal("登录数据、剩余秒数、UTC 时间或原对象不一致")
			}
			response := LoginResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatal("登录成功响应编码失败")
			}
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.UseNumber()
			var body map[string]any
			if err := decoder.Decode(&body); err != nil || len(body) != 2 || body["request_id"] != response.RequestID {
				t.Fatal("登录响应外层结构错误")
			}
			object, valid := body["data"].(map[string]any)
			if !valid || object["access_token"] != result.AccessToken.Token || object["expires_in"] != json.Number("899") {
				t.Fatal("访问令牌或有效期字段错误")
			}
			expectedFields := 9
			if clientType == "native" {
				expectedFields++
				if object["refresh_token"] != result.RefreshToken {
					t.Fatal("原生响应未明确交付刷新原文")
				}
			} else if _, exists := object["refresh_token"]; exists || strings.Contains(string(encoded), result.RefreshToken) {
				t.Fatal("Web JSON 交付了刷新原文")
			}
			if len(object) != expectedFields || object["root_entry_id"] != "17" || object["quota_bytes"] != json.Number("100") || object["used_bytes"] != json.Number("60") || object["reserved_bytes"] != json.Number("40") {
				t.Fatal("登录数据字段或复用的配额字段错误")
			}
			user, userValid := object["user"].(map[string]any)
			device, deviceValid := object["device"].(map[string]any)
			session, sessionValid := object["session"].(map[string]any)
			if !userValid || !deviceValid || !sessionValid || len(user) != 3 || len(device) != 3 || len(session) != 3 ||
				user["id"] != "7" || user["display_name"] != profile.DisplayName || device["id"] != "13" || device["name"] != result.Device.Name ||
				device["platform"] != "WINDOWS" || session["id"] != "11" || session["device_id"] != "13" {
				t.Fatal("用户、设备、会话摘要或原文名称错误")
			}
			if session["expires_at"] != result.Session.ExpiresAt.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano) {
				t.Fatal("会话截止时间未使用 UTC 微秒精度")
			}
			data.Device.Name, data.User.DisplayName = "修改 DTO", "修改 DTO"
			if *result != beforeResult || *profile != beforeProfile {
				t.Fatal("修改登录 DTO 影响了服务原对象")
			}
		})
	}
}

func TestNewLoginDataTimeAndIntegerBoundaries(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		change  func(*domain.LoginTokenResult, *domain.UserProfile, *time.Time)
		seconds int64
	}{
		{name: "exact seconds", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, now *time.Time) {
			result.AccessToken.ExpiresAt = now.Add(10 * time.Second)
		}, seconds: 10},
		{name: "fractional remaining", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, now *time.Time) {
			result.AccessToken.ExpiresAt = now.Add(1500 * time.Millisecond)
		}, seconds: 1},
		{name: "less than one second", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, now *time.Time) {
			result.AccessToken.ExpiresAt = now.Add(time.Nanosecond)
		}},
		{name: "same instant in another zone", change: func(_ *domain.LoginTokenResult, _ *domain.UserProfile, now *time.Time) {
			*now = now.In(time.FixedZone("UTC-5", -5*60*60))
		}, seconds: 899},
		{name: "maximum ids", change: func(result *domain.LoginTokenResult, profile *domain.UserProfile, _ *time.Time) {
			profile.ID, profile.RootEntryID = ^uint64(0), ^uint64(0)
			result.Device.ID, result.Device.UserID = ^uint64(0), profile.ID
			result.Session.ID, result.Session.UserID, result.Session.DeviceID = ^uint64(0), profile.ID, result.Device.ID
		}, seconds: 899},
		{name: "maximum safe bytes", change: func(_ *domain.LoginTokenResult, profile *domain.UserProfile, _ *time.Time) {
			profile.QuotaBytes, profile.UsedBytes, profile.ReservedBytes = maxJSONSafeInteger, maxJSONSafeInteger, 0
		}, seconds: 899},
		{name: "microsecond session time", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, now *time.Time) {
			result.Session.ExpiresAt = now.Add(time.Hour).Add(789 * time.Nanosecond)
		}, seconds: 899},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result, profile, now := loginDataFixture()
			testCase.change(result, profile, &now)
			data, err := NewLoginData(result, profile, "native", now)
			if err != nil || data == nil || data.ExpiresIn != testCase.seconds || data.User.ID != strconv.FormatUint(profile.ID, 10) ||
				data.RootEntryID != strconv.FormatUint(profile.RootEntryID, 10) || data.Device.ID != strconv.FormatUint(result.Device.ID, 10) ||
				data.Session.ID != strconv.FormatUint(result.Session.ID, 10) || data.Session.DeviceID != data.Device.ID || data.Session.ExpiresAt.Nanosecond()%1000 != 0 {
				t.Fatal("时间、整数或完整 ID 边界映射失败")
			}
		})
	}
}

func TestNewLoginDataFailures(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		change func(*domain.LoginTokenResult, *domain.UserProfile, *time.Time)
		want   error
	}{
		{name: "zero session", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, _ *time.Time) { result.Session.ID = 0 }, want: domain.ErrInvalidSessionData},
		{name: "zero device", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, _ *time.Time) { result.Device.ID = 0 }, want: domain.ErrInvalidSessionData},
		{name: "wrong session owner", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, _ *time.Time) { result.Session.UserID = 99 }, want: domain.ErrInvalidSessionData},
		{name: "wrong device owner", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, _ *time.Time) { result.Device.UserID = 99 }, want: domain.ErrInvalidSessionData},
		{name: "wrong device id", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, _ *time.Time) {
			result.Session.DeviceID = 99
		}, want: domain.ErrInvalidSessionData},
		{name: "revoked device", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, now *time.Time) {
			revoked := *now
			result.Device.RevokedAt = &revoked
		}, want: domain.ErrDeviceRevoked},
		{name: "empty access", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, _ *time.Time) {
			result.AccessToken.Token = ""
		}},
		{name: "empty refresh", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, _ *time.Time) { result.RefreshToken = "" }},
		{name: "zero response time", change: func(_ *domain.LoginTokenResult, _ *domain.UserProfile, now *time.Time) { *now = time.Time{} }},
		{name: "zero session expiry", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, _ *time.Time) {
			result.Session.ExpiresAt = time.Time{}
		}, want: domain.ErrInvalidSessionData},
		{name: "session date outside database range", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, _ *time.Time) {
			result.Session.ExpiresAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		}, want: domain.ErrInvalidSessionData},
		{name: "zero access expiry", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, _ *time.Time) {
			result.AccessToken.ExpiresAt = time.Time{}
		}, want: domain.ErrInvalidSessionData},
		{name: "access outlives session", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, _ *time.Time) {
			result.AccessToken.ExpiresAt = result.Session.ExpiresAt.Add(time.Second)
		}, want: domain.ErrInvalidSessionData},
		{name: "session expired", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, now *time.Time) {
			result.Session.ExpiresAt, result.AccessToken.ExpiresAt = *now, now.Add(-time.Second)
		}, want: domain.ErrSessionExpired},
		{name: "access expired at boundary", change: func(result *domain.LoginTokenResult, _ *domain.UserProfile, now *time.Time) {
			result.AccessToken.ExpiresAt = *now
		}, want: domain.ErrAccessTokenExpired},
		{name: "unsafe profile bytes", change: func(_ *domain.LoginTokenResult, profile *domain.UserProfile, _ *time.Time) {
			profile.QuotaBytes = maxJSONSafeInteger + 1
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result, profile, now := loginDataFixture()
			testCase.change(result, profile, &now)
			data, err := NewLoginData(result, profile, "native", now)
			if data != nil || err == nil || testCase.want != nil && !errors.Is(err, testCase.want) {
				t.Fatal("异常结果未返回空 DTO 或错误原因丢失")
			}
			if strings.Contains(err.Error(), "fixture-access-token") || strings.Contains(err.Error(), "fixture-refresh-token") {
				t.Fatal("转换错误回显了凭证")
			}
		})
	}
	for _, input := range []string{"nil result", "nil profile", "unknown client", "empty client"} {
		t.Run(input, func(t *testing.T) {
			result, profile, now := loginDataFixture()
			clientType := "native"
			switch input {
			case "nil result":
				result = nil
			case "nil profile":
				profile = nil
			case "unknown client":
				clientType = "unknown"
			case "empty client":
				clientType = ""
			}
			if data, err := NewLoginData(result, profile, clientType, now); data != nil || err == nil {
				t.Fatal("无效输入未被拒绝")
			}
		})
	}
}

func TestNewLoginDataRealJWT(t *testing.T) {
	result, profile, _ := loginDataFixture()
	cfg := config.JWTConfig{SigningKey: bytes.Repeat([]byte{7}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test", AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour}
	signer, err := service.NewAccessTokenSigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result.Session.ExpiresAt = time.Now().UTC().Add(time.Hour)
	token, err := signer.Issue(result.Session)
	if err != nil {
		t.Fatal("真实 JWT 签发失败")
	}
	result.AccessToken = *token
	result.RefreshToken, _ = service.GenerateRefreshToken()
	verifier, err := service.NewAccessTokenVerifier(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, clientType := range []string{"web", "native"} {
		data, err := NewLoginData(result, profile, clientType, time.Now())
		if err != nil || data == nil {
			t.Fatal("真实凭证映射失败")
		}
		identity, err := verifier.Verify(data.AccessToken)
		if err != nil || identity == nil || identity.UserID != profile.ID || strconv.FormatUint(identity.SessionID, 10) != data.Session.ID {
			t.Fatal("响应中的 JWT 与用户会话不一致")
		}
	}
}
