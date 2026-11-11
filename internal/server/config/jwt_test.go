package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// clearJWTEnvironment 让用例覆盖真正未设置变量的情况，并由 testing 恢复原环境。
func clearJWTEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"CLOUDNEST_JWT_SECRET", "CLOUDNEST_JWT_ISSUER", "CLOUDNEST_JWT_AUDIENCE", "ACCESS_TOKEN_TTL", "REFRESH_TOKEN_TTL"} {
		t.Setenv(name, "fixture")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadJWTConfigDefaults(t *testing.T) {
	clearJWTEnvironment(t)
	key := bytes.Repeat([]byte{0x42}, 32)
	secretText := base64.RawURLEncoding.EncodeToString(key)
	jwtConfig, err := LoadJWTConfig(secretText)
	if err != nil || !bytes.Equal(jwtConfig.SigningKey, key) || jwtConfig.Issuer != "cloudnest" || jwtConfig.Audience != "cloudnest-api" || jwtConfig.AccessTokenTTL != 15*time.Minute || jwtConfig.RefreshTokenTTL != 30*24*time.Hour {
		t.Fatalf("默认 JWT 配置或本地密钥读取错误: %v", err)
	}
	// 密钥排除于 JSON，避免未来误把内部配置整体编码后带出。
	encoded, err := json.Marshal(jwtConfig)
	if err != nil || bytes.Contains(encoded, []byte("SigningKey")) || bytes.Contains(encoded, []byte(secretText)) || bytes.Contains(encoded, key) {
		t.Fatal("配置 JSON 包含签名密钥")
	}
}

func TestLoadJWTConfigOverrides(t *testing.T) {
	for _, keySize := range []int{32, 48, 64} {
		t.Run(strconv.Itoa(keySize)+" byte key", func(t *testing.T) {
			clearJWTEnvironment(t)
			key := bytes.Repeat([]byte{0x53}, keySize)
			t.Setenv("CLOUDNEST_JWT_SECRET", base64.RawURLEncoding.EncodeToString(key))
			t.Setenv("CLOUDNEST_JWT_ISSUER", "CloudNest.Custom")
			t.Setenv("CLOUDNEST_JWT_AUDIENCE", "urn:cloudnest:API")
			t.Setenv("ACCESS_TOKEN_TTL", "1.5m")
			t.Setenv("REFRESH_TOKEN_TTL", "24h")
			jwtConfig, err := LoadJWTConfig("invalid-local-fixture")
			if err != nil || !bytes.Equal(jwtConfig.SigningKey, key) || jwtConfig.Issuer != "CloudNest.Custom" || jwtConfig.Audience != "urn:cloudnest:API" || jwtConfig.AccessTokenTTL != 90*time.Second || jwtConfig.RefreshTokenTTL != 24*time.Hour {
				t.Fatalf("环境覆盖或时长转换错误: %v", err)
			}
			// 每次解码取得独立密钥，防止调用方修改配置影响后续加载。
			jwtConfig.SigningKey[0] = 0
			reloaded, err := LoadJWTConfig("")
			if err != nil || !bytes.Equal(reloaded.SigningKey, key) {
				t.Fatal("配置加载之间共享了可变密钥")
			}
		})
	}
}

func TestLoadJWTConfigInvalidSecret(t *testing.T) {
	valid := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	for _, testCase := range []struct {
		name, localSecret, environment string
		setEnvironment                 bool
	}{
		{name: "missing all"},
		{name: "explicit empty override", localSecret: valid, setEnvironment: true},
		{name: "short decoded key", localSecret: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 31))},
		{name: "bad local text", localSecret: "private-bad-key"},
		{name: "invalid environment wins", localSecret: valid, environment: "private-bad-key", setEnvironment: true},
		{name: "padded base64", localSecret: valid + "="},
		{name: "leading whitespace", localSecret: " " + valid},
		{name: "trailing whitespace", localSecret: valid + " "},
		{name: "line break", localSecret: valid[:10] + "\r\n" + valid[10:]},
		{name: "standard base64 alphabet", localSecret: base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, 32))},
		{name: "noncanonical trailing bits", localSecret: valid[:42] + "J"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			clearJWTEnvironment(t)
			if testCase.setEnvironment {
				t.Setenv("CLOUDNEST_JWT_SECRET", testCase.environment)
			}
			jwtConfig, err := LoadJWTConfig(testCase.localSecret)
			if err == nil || !reflect.DeepEqual(jwtConfig, JWTConfig{}) || !strings.Contains(err.Error(), "CLOUDNEST_JWT_SECRET") || strings.Contains(err.Error(), "private-bad-key") || strings.Contains(err.Error(), valid) {
				t.Fatal("无效密钥未被拒绝、返回了部分配置或错误泄漏输入")
			}
		})
	}
}

func TestLoadJWTConfigInvalidFields(t *testing.T) {
	secretText := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	for _, testCase := range []struct {
		name, variable, value string
	}{
		{name: "empty issuer", variable: "CLOUDNEST_JWT_ISSUER"},
		{name: "issuer whitespace", variable: "CLOUDNEST_JWT_ISSUER", value: " private-issuer "},
		{name: "issuer control", variable: "CLOUDNEST_JWT_ISSUER", value: "private-issuer\n"},
		{name: "empty audience", variable: "CLOUDNEST_JWT_AUDIENCE"},
		{name: "audience control", variable: "CLOUDNEST_JWT_AUDIENCE", value: "private-audience\tvalue"},
		{name: "empty access ttl", variable: "ACCESS_TOKEN_TTL"},
		{name: "zero access ttl", variable: "ACCESS_TOKEN_TTL", value: "0s"},
		{name: "negative access ttl", variable: "ACCESS_TOKEN_TTL", value: "-1s"},
		{name: "fractional second", variable: "ACCESS_TOKEN_TTL", value: "1.5s"},
		{name: "missing unit", variable: "ACCESS_TOKEN_TTL", value: "900"},
		{name: "duration overflow", variable: "ACCESS_TOKEN_TTL", value: "999999999999999999h"},
		{name: "duration whitespace", variable: "ACCESS_TOKEN_TTL", value: " 15m"},
		{name: "empty refresh ttl", variable: "REFRESH_TOKEN_TTL"},
		{name: "invalid refresh ttl", variable: "REFRESH_TOKEN_TTL", value: "private-duration"},
		{name: "day unit unsupported", variable: "REFRESH_TOKEN_TTL", value: "30d"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			clearJWTEnvironment(t)
			t.Setenv(testCase.variable, testCase.value)
			jwtConfig, err := LoadJWTConfig(secretText)
			if err == nil || !reflect.DeepEqual(jwtConfig, JWTConfig{}) || !strings.Contains(err.Error(), testCase.variable) || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), secretText) {
				t.Fatal("无效字段返回了部分配置或未正确定位错误")
			}
		})
	}
}

func TestValidateJWTIdentityRawValues(t *testing.T) {
	// Windows 环境变量会转换无效 UTF-8，NUL 则在系统调用前被拒绝，直接检查校验边界。
	for _, value := range []string{string([]byte{0xff}), "private-identity\x00"} {
		if err := validateJWTIdentity("CLOUDNEST_JWT_ISSUER", value); err == nil || strings.Contains(err.Error(), "private-identity") {
			t.Error("原始异常文本未被拒绝或被错误消息回显")
		}
	}
	if err := validateJWTIdentity("CLOUDNEST_JWT_ISSUER", "我的云盘"); err != nil {
		t.Error("合法 UTF-8 标识被拒绝")
	}
}
