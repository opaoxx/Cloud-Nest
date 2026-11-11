package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func accessTokenTestConfig() config.JWTConfig {
	return config.JWTConfig{
		SigningKey:      bytes.Repeat([]byte{0x53}, 32),
		Issuer:          "cloudnest",
		Audience:        "cloudnest-api",
		AccessTokenTTL:  15 * time.Minute,
		RefreshTokenTTL: 30 * 24 * time.Hour,
	}
}

func parseIssuedAccessToken(token string, jwtConfig config.JWTConfig, now time.Time) (*accessTokenClaims, error) {
	claims := &accessTokenClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) { return jwtConfig.SigningKey, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(jwtConfig.Issuer), jwt.WithAudience(jwtConfig.Audience),
		jwt.WithTimeFunc(func() time.Time { return now }), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	return claims, err
}

func TestAccessTokenSignerClaimsAndSignature(t *testing.T) {
	jwtConfig := accessTokenTestConfig()
	signer, err := NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 8, 0, 0, 700000000, time.UTC)
	signer.now = func() time.Time { return now }
	for _, userID := range []uint64{1, ^uint64(0)} {
		session := domain.LoginSession{ID: ^uint64(0), UserID: userID, DeviceID: 7, ExpiresAt: now.Add(time.Hour)}
		result, err := signer.Issue(session)
		if err != nil || result == nil || result.Token == "" || result.ExpiresAt != now.Truncate(time.Second).Add(15*time.Minute) {
			t.Fatalf("签发结果或到期时间错误: %v", err)
		}
		claims, err := parseIssuedAccessToken(result.Token, jwtConfig, now)
		if err != nil {
			t.Fatalf("JWT 签名或声明校验失败: %v", err)
		}
		if claims.Subject != strconv.FormatUint(userID, 10) || claims.SessionID != "18446744073709551615" {
			t.Fatal("用户或会话 ID 没有完整保留")
		}
		if claims.Issuer != jwtConfig.Issuer || len(claims.Audience) != 1 || claims.Audience[0] != jwtConfig.Audience {
			t.Fatal("签发者或接收方字段错误")
		}
		if claims.IssuedAt == nil || claims.ExpiresAt == nil {
			t.Fatal("缺少签发或到期时间")
		}
		if !claims.IssuedAt.Time.Equal(now.Truncate(time.Second)) || !claims.ExpiresAt.Time.Equal(result.ExpiresAt) {
			t.Fatal("JWT 时间与返回结果不对应")
		}
		parts := strings.Split(result.Token, ".")
		if len(parts) != 3 {
			t.Fatal("JWT 须有三个部分")
		}
		var header map[string]string
		headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil || json.Unmarshal(headerBytes, &header) != nil || header["alg"] != "HS256" || header["typ"] != "JWT" {
			t.Fatal("JWT 头部或固定算法错误")
		}
		var payload map[string]json.RawMessage
		payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil || json.Unmarshal(payloadBytes, &payload) != nil || len(payload) != 6 {
			t.Fatal("JWT 声明数量或编码错误")
		}
		for _, field := range []string{"iss", "aud", "sub", "sid", "iat", "exp"} {
			if _, exists := payload[field]; !exists {
				t.Error("JWT 缺少约定声明字段")
			}
		}
		// 使用标准库独立计算签名，核对实际签名覆盖 JWT 头和正文。
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		mac := hmac.New(sha256.New, jwtConfig.SigningKey)
		_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
		if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
			t.Fatal("JWT 签名与独立 HMAC-SHA256 计算不一致")
		}
		encoded, err := json.Marshal(result)
		if err != nil || bytes.Contains(encoded, []byte(result.Token)) {
			t.Fatal("内部结果 JSON 直接带出了令牌原文")
		}
	}
}

func TestAccessTokenSignerExpiry(t *testing.T) {
	now := time.Date(2026, 11, 11, 8, 0, 0, 700000000, time.UTC)
	signer, err := NewAccessTokenSigner(accessTokenTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	signer.now = func() time.Time { return now }
	for _, testCase := range []struct {
		name      string
		expiresAt time.Time
		want      time.Time
	}{
		{name: "normal window", expiresAt: now.Add(time.Hour), want: now.Truncate(time.Second).Add(15 * time.Minute)},
		{name: "shorter session", expiresAt: now.Add(50*time.Second + 123456*time.Nanosecond), want: now.Truncate(time.Second).Add(50 * time.Second)},
		{name: "equal boundary", expiresAt: now.Truncate(time.Second).Add(15 * time.Minute), want: now.Truncate(time.Second).Add(15 * time.Minute)},
		{name: "next whole second", expiresAt: now.Truncate(time.Second).Add(time.Second), want: now.Truncate(time.Second).Add(time.Second)},
		{name: "expired", expiresAt: now.Add(-time.Nanosecond)},
		{name: "exact current time", expiresAt: now},
		{name: "too close to encode expiry", expiresAt: now.Add(100 * time.Millisecond)},
		{name: "zero expiry"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := signer.Issue(domain.LoginSession{ID: 1, UserID: 2, DeviceID: 3, ExpiresAt: testCase.expiresAt})
			if testCase.want.IsZero() {
				if result != nil || !errors.Is(err, domain.ErrSessionExpired) {
					t.Fatal("到期会话仍签发了令牌")
				}
			} else if err != nil || result == nil || result.ExpiresAt != testCase.want || result.ExpiresAt.After(testCase.expiresAt) {
				t.Fatal("访问令牌超过会话有效期或整秒边界错误")
			}
		})
	}
}

func TestAccessTokenSignerInvalidInput(t *testing.T) {
	for _, mutate := range []func(*config.JWTConfig){
		func(value *config.JWTConfig) { value.SigningKey = value.SigningKey[:31] },
		func(value *config.JWTConfig) { value.Issuer = "" },
		func(value *config.JWTConfig) { value.Audience = "private-invalid\n" },
		func(value *config.JWTConfig) { value.AccessTokenTTL = 0 },
		func(value *config.JWTConfig) { value.AccessTokenTTL = -time.Second },
		func(value *config.JWTConfig) { value.AccessTokenTTL = 1500 * time.Millisecond },
		func(value *config.JWTConfig) { value.RefreshTokenTTL = 0 },
	} {
		jwtConfig := accessTokenTestConfig()
		mutate(&jwtConfig)
		if signer, err := NewAccessTokenSigner(jwtConfig); signer != nil || err == nil || strings.Contains(err.Error(), "private-invalid") {
			t.Fatal("无效配置构造了签发器或错误泄漏输入")
		}
	}
	signer, err := NewAccessTokenSigner(accessTokenTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []domain.LoginSession{
		{UserID: 2, DeviceID: 3}, {ID: 1, DeviceID: 3}, {ID: 1, UserID: 2},
	} {
		if result, err := signer.Issue(session); result != nil || !errors.Is(err, domain.ErrInvalidSessionData) {
			t.Fatal("缺少标识的会话仍签发了令牌")
		}
	}
}

func TestAccessTokenSignerKeySnapshotAndVerification(t *testing.T) {
	jwtConfig := accessTokenTestConfig()
	original := jwtConfig
	original.SigningKey = bytes.Clone(jwtConfig.SigningKey)
	signer, err := NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	signer.now = func() time.Time { return now }
	jwtConfig.SigningKey[0] ^= 0xff
	jwtConfig.Issuer = "changed-issuer"
	jwtConfig.AccessTokenTTL = time.Hour
	result, err := signer.Issue(domain.LoginSession{ID: 1, UserID: 2, DeviceID: 3, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseIssuedAccessToken(result.Token, original, now); err != nil || result.ExpiresAt != now.Add(15*time.Minute) {
		t.Fatal("修改调用方配置影响了已构造的签发器")
	}
	parts := strings.Split(result.Token, ".")
	tamperedBody := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"999","sid":"1","iss":"cloudnest","aud":["cloudnest-api"],"exp":9999999999}`))
	for _, testCase := range []struct {
		name      string
		token     string
		cfg       config.JWTConfig
		now       time.Time
		wantError error
	}{
		{name: "wrong key", token: result.Token, cfg: config.JWTConfig{SigningKey: jwtConfig.SigningKey, Issuer: original.Issuer, Audience: original.Audience}, now: now, wantError: jwt.ErrTokenSignatureInvalid},
		{name: "tampered body", token: parts[0] + "." + tamperedBody + "." + parts[2], cfg: original, now: now, wantError: jwt.ErrTokenSignatureInvalid},
		{name: "expired", token: result.Token, cfg: original, now: result.ExpiresAt, wantError: jwt.ErrTokenExpired},
		{name: "wrong audience", token: result.Token, cfg: config.JWTConfig{SigningKey: original.SigningKey, Issuer: original.Issuer, Audience: "another-api"}, now: now, wantError: jwt.ErrTokenInvalidAudience},
		{name: "wrong issuer", token: result.Token, cfg: config.JWTConfig{SigningKey: original.SigningKey, Issuer: "another-issuer", Audience: original.Audience}, now: now, wantError: jwt.ErrTokenInvalidIssuer},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseIssuedAccessToken(testCase.token, testCase.cfg, testCase.now); !errors.Is(err, testCase.wantError) {
				t.Error("JWT 验证没有返回预期的签名或声明错误")
			}
		})
	}
}

func TestAccessTokenSignerConcurrent(t *testing.T) {
	jwtConfig := accessTokenTestConfig()
	signer, err := NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	signer.now = func() time.Time { return now }
	type outcome struct {
		session domain.LoginSession
		token   *domain.AccessToken
		err     error
	}
	results := make(chan outcome, 32)
	var workers sync.WaitGroup
	for index := range cap(results) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			session := domain.LoginSession{ID: uint64(index + 1), UserID: uint64(index + 100), DeviceID: 7, ExpiresAt: now.Add(time.Hour)}
			result, err := signer.Issue(session)
			results <- outcome{session: session, token: result, err: err}
		}()
	}
	workers.Wait()
	close(results)
	for result := range results {
		if result.err != nil || result.token == nil {
			t.Fatal("并发签发失败")
		}
		claims, err := parseIssuedAccessToken(result.token.Token, jwtConfig, now)
		if err != nil || claims.Subject != strconv.FormatUint(result.session.UserID, 10) || claims.SessionID != strconv.FormatUint(result.session.ID, 10) {
			t.Fatal("并发签发的身份字段不一致")
		}
	}
}

func TestAccessTokenSignerPersistedKey(t *testing.T) {
	for name, value := range map[string]string{"CLOUDNEST_JWT_ISSUER": "cloudnest", "CLOUDNEST_JWT_AUDIENCE": "cloudnest-api", "ACCESS_TOKEN_TTL": "15m", "REFRESH_TOKEN_TTL": "720h"} {
		t.Setenv(name, value)
	}
	t.Setenv("CLOUDNEST_JWT_SECRET", "fixture")
	if err := os.Unsetenv("CLOUDNEST_JWT_SECRET"); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(t.TempDir(), "CloudNest", "jwt-secret")
	now := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	session := domain.LoginSession{ID: 1, UserID: 2, DeviceID: 3, ExpiresAt: now.Add(time.Hour)}
	var previous string
	for range 2 {
		jwtConfig, err := config.LoadOrCreateJWTConfig(secretPath)
		if err != nil {
			t.Fatal(err)
		}
		signer, err := NewAccessTokenSigner(jwtConfig)
		if err != nil {
			t.Fatal(err)
		}
		signer.now = func() time.Time { return now }
		result, err := signer.Issue(session)
		if err != nil || (previous != "" && previous != result.Token) {
			t.Fatal("复用持久化密钥后签名结果改变")
		}
		previous = result.Token
	}
}
