package service

import (
	"bytes"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// accessTokenClaims 组合 JWT 注册字段和项目会话标识，ID 保持十进制字符串。
type accessTokenClaims struct {
	// SessionID 是后续数据库会话查询使用的 sid。
	SessionID string `json:"sid"`
	// RegisteredClaims 保存 iss、aud、sub、iat、exp 等标准字段。
	jwt.RegisteredClaims
}

// AccessTokenSigner 保存签发访问令牌所需的稳定配置。
type AccessTokenSigner struct {
	// signingKey 是签发器独立保存的密钥字节。
	signingKey []byte
	// issuer 是写入 iss 的签发者标识。
	issuer string
	// audience 是写入 aud 的 API 接收方标识。
	audience string
	// lifetime 是配置的访问令牌有效时长。
	lifetime time.Duration
	// now 提供当前时间，生产构造器绑定 time.Now。
	now func() time.Time
}

// NewAccessTokenSigner 校验配置并复制签名密钥，供后续请求复用。
func NewAccessTokenSigner(jwtConfig config.JWTConfig) (*AccessTokenSigner, error) {
	// 1. 沿用配置类型的校验规则。
	if err := jwtConfig.Validate(); err != nil {
		return nil, err
	}

	// 2. 复制签发所需字段，绑定生产时钟。
	return &AccessTokenSigner{
		signingKey: bytes.Clone(jwtConfig.SigningKey),
		issuer:     jwtConfig.Issuer,
		audience:   jwtConfig.Audience,
		lifetime:   jwtConfig.AccessTokenTTL,
		now:        time.Now,
	}, nil
}

// Issue 使用已提交的登录会话签发访问令牌，截止时间受会话到期时间限制。
func (signer *AccessTokenSigner) Issue(session domain.LoginSession) (*domain.AccessToken, error) {
	// 1. 检查会话标识，取得本次签发时间。
	if session.ID == 0 || session.UserID == 0 || session.DeviceID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	now := signer.now().UTC()
	issuedAt := now.Truncate(time.Second)

	// 2. 计算令牌截止时间，限制在会话有效期内。
	expiresAt := issuedAt.Add(signer.lifetime)
	sessionExpiry := session.ExpiresAt.UTC().Truncate(time.Second)
	if sessionExpiry.Before(expiresAt) {
		expiresAt = sessionExpiry
	}
	if !expiresAt.After(now) {
		return nil, domain.ErrSessionExpired
	}

	// 3. 组装用户、会话、签发者、接收方和时间字段。
	claims := accessTokenClaims{
		SessionID: strconv.FormatUint(session.ID, 10),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    signer.issuer,
			Subject:   strconv.FormatUint(session.UserID, 10),
			Audience:  jwt.ClaimStrings{signer.audience},
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}

	// 4. 使用 HS256 签名，返回令牌文本与到期时间。
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(signer.signingKey)
	if err != nil {
		return nil, fmt.Errorf("签发访问令牌失败: %w", err)
	}
	return &domain.AccessToken{Token: token, ExpiresAt: expiresAt}, nil
}
