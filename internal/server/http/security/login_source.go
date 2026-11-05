// Package security 提供 HTTP 层的请求来源约束，不负责用户身份认证。
package security

import "net/http"

// ClientHeader 是请求中显式声明 Web 或原生凭证交付模式的头名称。
const ClientHeader = "X-CloudNest-Client"

// LoginSourcePolicy 保存启动时已校验的前端来源白名单。
type LoginSourcePolicy struct {
	// allowedOrigins 在构造时复制，此后只读，不按路径或域名后缀匹配。
	allowedOrigins map[string]bool
}

// NewLoginSourcePolicy 复制规范化来源列表，不读取环境变量或执行网络访问。
func NewLoginSourcePolicy(allowedOrigins []string) *LoginSourcePolicy {
	// 1. 复制白名单，避免调用方之后修改原切片影响运行规则。
	originSet := make(map[string]bool, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		originSet[origin] = true
	}
	return &LoginSourcePolicy{allowedOrigins: originSet}
}

// Allows 检查请求头与已解析的客户端类型是否符合登录来源规则，不修改请求。
// clientType 只能为 web 或 native；通过不代表该客户端或账号已被认证。
func (policy *LoginSourcePolicy) Allows(request *http.Request, clientType string) bool {
	// 1. 检查客户端类型与单个客户端声明头是否一致。
	if request == nil || (clientType != "web" && clientType != "native") {
		return false
	}
	clientHeaders := request.Header.Values(ClientHeader)
	if len(clientHeaders) != 1 || clientHeaders[0] != clientType {
		return false
	}
	origins := request.Header.Values("Origin")

	// 2. Web 请求严格匹配白名单内的单个 Origin。
	if clientType == "web" {
		return len(origins) == 1 && origins[0] != "" && origins[0] != "null" && origins[0] != "*" && policy.allowedOrigins[origins[0]]
	}

	// 3. 原生请求要求不携带 Origin。
	if len(origins) != 0 {
		return false
	}

	// 4. 返回校验结果，由 Handler 决定是否继续。
	return true
}
