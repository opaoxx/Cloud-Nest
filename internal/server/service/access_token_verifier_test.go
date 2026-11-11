package service

import (
	"bytes"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func verifierTestClaims(now time.Time) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": "cloudnest", "aud": []string{"cloudnest-api"},
		"sub": "7", "sid": "11", "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Minute).Unix(),
	}
}

func signVerifierTestClaims(t *testing.T, claims jwt.MapClaims, method jwt.SigningMethod, key any) string {
	t.Helper()
	text, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatal("生成测试用 JWT 失败")
	}
	return text
}

func TestAccessTokenVerifierRoundTrip(t *testing.T) {
	jwtConfig := accessTokenTestConfig()
	signer, err := NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewAccessTokenVerifier(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 9, 0, 0, 123456000, time.UTC)
	signer.now = func() time.Time { return now }
	verifier.now = func() time.Time { return now }
	for _, identifier := range []uint64{1, ^uint64(0)} {
		session := domain.LoginSession{ID: identifier, UserID: identifier, DeviceID: 3, ExpiresAt: now.Add(time.Hour)}
		issued, err := signer.Issue(session)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := verifier.Verify(issued.Token)
		if err != nil || identity == nil || identity.UserID != identifier || identity.SessionID != identifier || !identity.ExpiresAt.Equal(issued.ExpiresAt) || identity.ExpiresAt.Location() != time.UTC {
			t.Fatalf("签发到校验的闭环失败: %v", err)
		}
	}
	// aud 可以按 JWT 标准使用单字符串或包含本 API 的字符串数组。
	for _, audience := range []any{"cloudnest-api", []string{"another-api", "cloudnest-api"}} {
		claims := verifierTestClaims(now)
		claims["aud"] = audience
		claims["nbf"] = now.Unix()
		text := signVerifierTestClaims(t, claims, jwt.SigningMethodHS256, jwtConfig.SigningKey)
		if identity, err := verifier.Verify(text); err != nil || identity == nil {
			t.Fatal("合法 audience 或 nbf 边界被拒绝")
		}
	}
}

func TestAccessTokenVerifierInvalidClaims(t *testing.T) {
	jwtConfig := accessTokenTestConfig()
	verifier, err := NewAccessTokenVerifier(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	verifier.now = func() time.Time { return now }
	for _, testCase := range []struct {
		name   string
		mutate func(jwt.MapClaims)
		want   error
	}{
		{name: "wrong issuer", mutate: func(claims jwt.MapClaims) { claims["iss"] = "private-wrong-issuer" }},
		{name: "wrong audience", mutate: func(claims jwt.MapClaims) { claims["aud"] = "another-api" }},
		{name: "missing issuer", mutate: func(claims jwt.MapClaims) { delete(claims, "iss") }},
		{name: "missing audience", mutate: func(claims jwt.MapClaims) { delete(claims, "aud") }},
		{name: "missing expiry", mutate: func(claims jwt.MapClaims) { delete(claims, "exp") }},
		{name: "null expiry", mutate: func(claims jwt.MapClaims) { claims["exp"] = nil }},
		{name: "missing issued at", mutate: func(claims jwt.MapClaims) { delete(claims, "iat") }},
		{name: "invalid issued at type", mutate: func(claims jwt.MapClaims) { claims["iat"] = "private-time" }},
		{name: "future issued at", mutate: func(claims jwt.MapClaims) { claims["iat"] = now.Add(time.Second).Unix() }},
		{name: "future not before", mutate: func(claims jwt.MapClaims) { claims["nbf"] = now.Add(time.Second).Unix() }},
		{name: "expiry before issuance", mutate: func(claims jwt.MapClaims) { claims["exp"] = now.Add(-2 * time.Minute).Unix() }},
		{name: "missing subject", mutate: func(claims jwt.MapClaims) { delete(claims, "sub") }},
		{name: "missing session id", mutate: func(claims jwt.MapClaims) { delete(claims, "sid") }},
		{name: "numeric subject", mutate: func(claims jwt.MapClaims) { claims["sub"] = 7 }},
		{name: "numeric session id", mutate: func(claims jwt.MapClaims) { claims["sid"] = 11 }},
		{name: "uppercase subject name", mutate: func(claims jwt.MapClaims) { claims["SUB"] = claims["sub"]; delete(claims, "sub") }},
		{name: "uppercase session name", mutate: func(claims jwt.MapClaims) { claims["SID"] = claims["sid"]; delete(claims, "sid") }},
		{name: "uppercase issuer name", mutate: func(claims jwt.MapClaims) { claims["ISS"] = claims["iss"]; delete(claims, "iss") }},
		{name: "expired", mutate: func(claims jwt.MapClaims) { claims["exp"] = now.Add(-time.Second).Unix() }, want: domain.ErrAccessTokenExpired},
		{name: "exact expiry", mutate: func(claims jwt.MapClaims) { claims["exp"] = now.Unix() }, want: domain.ErrAccessTokenExpired},
		{name: "expired wrong issuer", mutate: func(claims jwt.MapClaims) { claims["exp"] = now.Unix(); claims["iss"] = "another-issuer" }},
		{name: "expired missing audience", mutate: func(claims jwt.MapClaims) { claims["exp"] = now.Unix(); delete(claims, "aud") }},
		{name: "expired future not before", mutate: func(claims jwt.MapClaims) { claims["exp"] = now.Unix(); claims["nbf"] = now.Add(time.Second).Unix() }},
		{name: "expired future issued at", mutate: func(claims jwt.MapClaims) { claims["exp"] = now.Unix(); claims["iat"] = now.Add(time.Second).Unix() }},
		{name: "expired invalid id", mutate: func(claims jwt.MapClaims) { claims["exp"] = now.Unix(); claims["sid"] = "0" }},
		{name: "expired bad issuer type", mutate: func(claims jwt.MapClaims) { claims["exp"] = now.Unix(); claims["iss"] = 1 }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			claims := verifierTestClaims(now)
			testCase.mutate(claims)
			text := signVerifierTestClaims(t, claims, jwt.SigningMethodHS256, jwtConfig.SigningKey)
			identity, err := verifier.Verify(text)
			want := testCase.want
			if want == nil {
				want = domain.ErrInvalidAccessToken
			}
			if identity != nil || err != want {
				t.Fatalf("声明错误未返回固定拒绝结果: %v", err)
			}
		})
	}
	for _, field := range []string{"sub", "sid"} {
		for _, value := range []string{"", "0", "01", "+1", "-1", " 1", "1 ", "1.0", "1e1", "18446744073709551616", "private-id"} {
			claims := verifierTestClaims(now)
			claims[field] = value
			text := signVerifierTestClaims(t, claims, jwt.SigningMethodHS256, jwtConfig.SigningKey)
			if identity, err := verifier.Verify(text); identity != nil || err != domain.ErrInvalidAccessToken {
				t.Error("非法十进制 ID 声明通过校验")
			}
		}
	}
}

func TestAccessTokenVerifierRejectsBadSignaturesAndText(t *testing.T) {
	jwtConfig := accessTokenTestConfig()
	verifier, err := NewAccessTokenVerifier(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	verifier.now = func() time.Time { return now }
	claims := verifierTestClaims(now)
	valid := signVerifierTestClaims(t, claims, jwt.SigningMethodHS256, jwtConfig.SigningKey)
	parts := strings.Split(valid, ".")
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	signature := parts[2]
	lastIndex := strings.IndexByte(alphabet, signature[len(signature)-1])
	// 只改变 Base64URL 的未使用尾位，严格解码应拒绝这个等价字节表示。
	noncanonical := signature[:len(signature)-1] + string(alphabet[lastIndex+1])
	for _, testCase := range []struct{ name, text string }{
		{name: "empty"},
		{name: "arbitrary text", text: "private-token-text"},
		{name: "missing part", text: parts[0] + "." + parts[1]},
		{name: "extra part", text: valid + ".extra"},
		{name: "bearer prefix", text: "Bearer " + valid},
		{name: "surrounding whitespace", text: " " + valid + " "},
		{name: "line break", text: valid + "\r\n"},
		{name: "padding", text: valid + "="},
		{name: "noncanonical signature", text: parts[0] + "." + parts[1] + "." + noncanonical},
		{name: "tampered body", text: parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"999"}`)) + "." + parts[2]},
		{name: "wrong key", text: signVerifierTestClaims(t, claims, jwt.SigningMethodHS256, bytes.Repeat([]byte{0x42}, 32))},
		{name: "hs384", text: signVerifierTestClaims(t, claims, jwt.SigningMethodHS384, jwtConfig.SigningKey)},
		{name: "hs512", text: signVerifierTestClaims(t, claims, jwt.SigningMethodHS512, jwtConfig.SigningKey)},
		{name: "none", text: signVerifierTestClaims(t, claims, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			identity, err := verifier.Verify(testCase.text)
			if identity != nil || err != domain.ErrInvalidAccessToken {
				t.Fatal("异常文本或签名未返回固定拒绝结果")
			}
		})
	}
}

func TestAccessTokenVerifierConfigurationSnapshot(t *testing.T) {
	jwtConfig := accessTokenTestConfig()
	invalidConfig := jwtConfig
	invalidConfig.SigningKey = nil
	if verifier, err := NewAccessTokenVerifier(invalidConfig); verifier != nil || err == nil {
		t.Fatal("无效配置构造了校验器")
	}
	signer, err := NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewAccessTokenVerifier(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	signer.now = func() time.Time { return now }
	verifier.now = func() time.Time { return now }
	jwtConfig.SigningKey[0] ^= 0xff
	jwtConfig.Issuer = "changed-issuer"
	jwtConfig.Audience = "changed-audience"
	issued, err := signer.Issue(domain.LoginSession{ID: 11, UserID: 7, DeviceID: 3, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := verifier.Verify(issued.Token)
	if err != nil || identity == nil || identity.UserID != 7 || identity.SessionID != 11 {
		t.Fatal("修改调用方配置影响了已构造的校验器")
	}
}

func TestAccessTokenVerifierConcurrent(t *testing.T) {
	jwtConfig := accessTokenTestConfig()
	signer, err := NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewAccessTokenVerifier(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	signer.now = func() time.Time { return now }
	verifier.now = func() time.Time { return now }
	type outcome struct {
		expectedID uint64
		identity   *domain.VerifiedAccessToken
		err        error
	}
	results := make(chan outcome, 32)
	var workers sync.WaitGroup
	for index := range cap(results) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			identifier := uint64(index + 1)
			issued, err := signer.Issue(domain.LoginSession{ID: identifier, UserID: identifier, DeviceID: 3, ExpiresAt: now.Add(time.Hour)})
			if err != nil {
				results <- outcome{expectedID: identifier, err: err}
				return
			}
			text := issued.Token
			if index%4 == 0 {
				text = ""
			}
			identity, err := verifier.Verify(text)
			results <- outcome{expectedID: identifier, identity: identity, err: err}
		}()
	}
	workers.Wait()
	close(results)
	for result := range results {
		if (result.expectedID-1)%4 == 0 {
			if result.identity != nil || result.err != domain.ErrInvalidAccessToken {
				t.Error("并发失败请求返回了身份")
			}
		} else if result.err != nil || result.identity == nil || result.identity.UserID != result.expectedID || result.identity.SessionID != result.expectedID {
			t.Error("并发校验的身份发生串号")
		}
	}
}
