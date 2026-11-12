package security

import (
	"net/http"
	"net/textproto"
	"strings"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

// RefreshCookieName 是 Web 刷新原文在提取与交付时共用的 Cookie 名称。
const RefreshCookieName = "cloudnest_refresh_token"

// RefreshToken 按客户端模式提取原文，调用方须先完成 DTO 与来源校验。
// 提取失败返回空字符串及固定 ErrInvalidRefreshToken，格式与授权由刷新服务检查。
func RefreshToken(request *http.Request, input dto.RefreshRequest) (string, error) {
	// 1. 检查请求及 DTO 字段组合。
	if request == nil || input.Validate() != nil {
		return "", domain.ErrInvalidRefreshToken
	}

	// 2. 原生模式返回正文中的刷新原文。
	if input.ClientType == "native" {
		return input.RefreshToken, nil
	}

	// 3. Web 模式确认同名 Cookie 唯一，再按标准库解析。
	// 先统计原始同名项，覆盖标准库可能跳过的无效值，避免从重复项中选取凭证。
	cookieCount := 0
	for _, line := range request.Header.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			name, _, _ := strings.Cut(textproto.TrimString(part), "=")
			if textproto.TrimString(name) == RefreshCookieName {
				cookieCount++
			}
		}
	}
	if cookieCount != 1 {
		return "", domain.ErrInvalidRefreshToken
	}
	cookies := request.CookiesNamed(RefreshCookieName)

	// 4. 检查解析结果，返回 Cookie 原文。
	if len(cookies) != 1 || cookies[0].Value == "" {
		return "", domain.ErrInvalidRefreshToken
	}
	return cookies[0].Value, nil
}
