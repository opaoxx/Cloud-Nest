package security

import (
	"errors"
	"net/http"
	"strings"
)

// ErrInvalidAuthorizationHeader 表示请求缺少单一、格式正确的 Bearer 凭证。
var ErrInvalidAuthorizationHeader = errors.New("请求需要有效的 Bearer 授权头")

// BearerToken 从单个 Authorization 请求头提取凭证，交给授权服务校验。
// 返回值保留令牌的大小写和内容，格式错误时返回空字符串及固定错误。
func BearerToken(request *http.Request) (string, error) {
	// 1. 读取单个 Authorization 请求头。
	if request == nil {
		return "", ErrInvalidAuthorizationHeader
	}
	values := request.Header.Values("Authorization")
	if len(values) != 1 {
		return "", ErrInvalidAuthorizationHeader
	}

	// 2. 识别 Bearer 方案，分离令牌文本。
	// 头值两端允许 HTTP 可选空白，方案与凭证之间要求一个或多个 ASCII 空格。
	headerValue := strings.Trim(values[0], " \t")
	scheme, credentials, separated := strings.Cut(headerValue, " ")
	if !separated || !strings.EqualFold(scheme, "Bearer") {
		return "", ErrInvalidAuthorizationHeader
	}
	tokenText := strings.TrimLeft(credentials, " ")

	// 3. 检查凭证字符，原样返回令牌。
	// Bearer 凭证允许字母、数字及 -._~+/，等号只能连续出现在末尾。
	credentialBody := strings.TrimRight(tokenText, "=")
	if credentialBody == "" {
		return "", ErrInvalidAuthorizationHeader
	}
	for _, character := range credentialBody {
		allowed := character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("-._~+/", character)
		if !allowed {
			return "", ErrInvalidAuthorizationHeader
		}
	}
	return tokenText, nil
}
