package config

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// JWTConfig 保存 Access Token 签发及后续刷新使用的配置。
type JWTConfig struct {
	// SigningKey 是 Base64URL 解码后的 HS256 密钥，按敏感数据处理。
	SigningKey []byte `json:"-"`
	// Issuer 是写入和检查 JWT iss 的签发者标识。
	Issuer string
	// Audience 是写入和检查 JWT aud 的 API 接收方标识。
	Audience string
	// AccessTokenTTL 是 Access Token 的有效时长。
	AccessTokenTTL time.Duration
	// RefreshTokenTTL 是会话与 Refresh Token 的有效时长。
	RefreshTokenTTL time.Duration
}

// LoadJWTConfig 读取环境变量，密钥未被环境覆盖时使用调用方提供的本地密钥文本。
// 密钥文本采用无填充 Base64URL；失败返回零值配置和标明配置项的错误。
func LoadJWTConfig(localSecret string) (JWTConfig, error) {
	// 1. 读取环境变量，以本地密钥作为未覆盖时的来源。
	secretText := envOrDefault("CLOUDNEST_JWT_SECRET", localSecret)
	jwtConfig := JWTConfig{
		Issuer:   envOrDefault("CLOUDNEST_JWT_ISSUER", "cloudnest"),
		Audience: envOrDefault("CLOUDNEST_JWT_AUDIENCE", "cloudnest-api"),
	}

	// 2. 解码并检查签名密钥。
	signingKey, err := base64.RawURLEncoding.Strict().DecodeString(secretText)
	if err != nil || len(signingKey) < 32 || base64.RawURLEncoding.EncodeToString(signingKey) != secretText {
		return JWTConfig{}, fmt.Errorf("CLOUDNEST_JWT_SECRET 须为标准无填充 Base64URL，解码后至少 32 字节")
	}

	// 3. 校验 issuer、audience 和两类令牌有效期。
	if err := validateJWTIdentity("CLOUDNEST_JWT_ISSUER", jwtConfig.Issuer); err != nil {
		return JWTConfig{}, err
	}
	if err := validateJWTIdentity("CLOUDNEST_JWT_AUDIENCE", jwtConfig.Audience); err != nil {
		return JWTConfig{}, err
	}
	accessTTL, err := parseTokenTTL("ACCESS_TOKEN_TTL", envOrDefault("ACCESS_TOKEN_TTL", "15m"))
	if err != nil {
		return JWTConfig{}, err
	}
	refreshTTL, err := parseTokenTTL("REFRESH_TOKEN_TTL", envOrDefault("REFRESH_TOKEN_TTL", "720h"))
	if err != nil {
		return JWTConfig{}, err
	}

	// 4. 返回配置，供后续令牌签发使用。
	jwtConfig.SigningKey = signingKey
	jwtConfig.AccessTokenTTL = accessTTL
	jwtConfig.RefreshTokenTTL = refreshTTL
	return jwtConfig, nil
}

// validateJWTIdentity 检查可准确匹配的身份配置，保留大小写及内部文字。
func validateJWTIdentity(variableName, value string) error {
	if !utf8.ValidString(value) || value == "" || value != strings.TrimSpace(value) || strings.ContainsFunc(value, unicode.IsControl) {
		return fmt.Errorf("%s 须为非空 UTF-8 文本，且不能包含首尾空白或控制字符", variableName)
	}
	return nil
}

// parseTokenTTL 将带单位的时长转换为供令牌使用的正整数秒时长。
func parseTokenTTL(variableName, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 || duration%time.Second != 0 {
		return 0, fmt.Errorf("%s 须为带单位的正整数秒时长，例如 15m 或 720h", variableName)
	}
	return duration, nil
}
