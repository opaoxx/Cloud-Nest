package service

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// AccessTokenVerifier 保存校验访问令牌所需的稳定配置。
type AccessTokenVerifier struct {
	// signingKey 是校验器独立保存的 HS256 密钥。
	signingKey []byte
	// issuer 是要求匹配的签发者标识。
	issuer string
	// audience 是要求匹配的 API 接收方标识。
	audience string
	// now 绑定生产时钟，测试可在并发调用前替换。
	now func() time.Time
}

// NewAccessTokenVerifier 校验配置并复制密钥，供后续请求复用。
func NewAccessTokenVerifier(jwtConfig config.JWTConfig) (*AccessTokenVerifier, error) {
	// 1. 沿用配置校验并复制校验所需的稳定字段。
	if err := jwtConfig.Validate(); err != nil {
		return nil, err
	}
	return &AccessTokenVerifier{
		signingKey: bytes.Clone(jwtConfig.SigningKey),
		issuer:     jwtConfig.Issuer,
		audience:   jwtConfig.Audience,
		now:        time.Now,
	}, nil
}

// Verify 校验 JWT 的签名和声明，返回后续数据库授权查询所需的身份标识。
func (verifier *AccessTokenVerifier) Verify(tokenText string) (*domain.VerifiedAccessToken, error) {
	// 1. 检查令牌文本，取得本次校验时间。
	if tokenText == "" || strings.ContainsAny(tokenText, " \t\r\n") {
		return nil, domain.ErrInvalidAccessToken
	}
	now := verifier.now().UTC()

	// 2. 验证固定算法、签名和标准声明。
	// MapClaims 按精确字段名取值，保持 JWT 声明名的大小写语义。
	claims := jwt.MapClaims{}
	parsed, parseError := jwt.ParseWithClaims(tokenText, claims,
		func(*jwt.Token) (any, error) { return verifier.signingKey, nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(verifier.issuer), jwt.WithAudience(verifier.audience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithStrictDecoding(),
		jwt.WithTimeFunc(func() time.Time { return now }))
	if parseError != nil {
		if !errors.Is(parseError, jwt.ErrTokenExpired) {
			return nil, domain.ErrInvalidAccessToken
		}
		// 库可合并多个声明错误，其他错误优先于单纯到期。
		for _, invalidReason := range []error{
			jwt.ErrTokenRequiredClaimMissing, jwt.ErrTokenNotValidYet, jwt.ErrTokenUsedBeforeIssued,
			jwt.ErrTokenInvalidAudience, jwt.ErrTokenInvalidIssuer, jwt.ErrInvalidType,
		} {
			if errors.Is(parseError, invalidReason) {
				return nil, domain.ErrInvalidAccessToken
			}
		}
	}

	// 3. 检查必填时间字段，解析用户与会话 ID。
	issuedAt, issuedAtError := claims.GetIssuedAt()
	expiresAt, expiresAtError := claims.GetExpirationTime()
	if issuedAtError != nil || expiresAtError != nil || issuedAt == nil || expiresAt == nil || !expiresAt.After(issuedAt.Time) {
		return nil, domain.ErrInvalidAccessToken
	}
	subject, subjectIsString := claims["sub"].(string)
	sessionText, sessionIsString := claims["sid"].(string)
	if !subjectIsString || !sessionIsString {
		return nil, domain.ErrInvalidAccessToken
	}
	userID, err := parseAccessTokenID(subject)
	if err != nil {
		return nil, err
	}
	sessionID, err := parseAccessTokenID(sessionText)
	if err != nil {
		return nil, err
	}

	// 4. 区分过期与其他错误，成功后返回已验证标识。
	if parseError != nil {
		return nil, domain.ErrAccessTokenExpired
	}
	if parsed == nil || !parsed.Valid {
		return nil, domain.ErrInvalidAccessToken
	}
	return &domain.VerifiedAccessToken{
		UserID:    userID,
		SessionID: sessionID,
		ExpiresAt: expiresAt.UTC(),
	}, nil
}

// parseAccessTokenID 还原非零 uint64，重新编码确保声明采用规范十进制形式。
func parseAccessTokenID(value string) (uint64, error) {
	identifier, err := strconv.ParseUint(value, 10, 64)
	if err != nil || identifier == 0 || strconv.FormatUint(identifier, 10) != value {
		return 0, domain.ErrInvalidAccessToken
	}
	return identifier, nil
}
