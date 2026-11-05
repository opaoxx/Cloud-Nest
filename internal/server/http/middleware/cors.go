package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const corsMethods = "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS"
const corsHeaders = "Authorization, Content-Type, Idempotency-Key, X-CloudNest-Client, X-Request-ID"

// CORS 使用启动时已校验的明确来源列表处理跨域请求，不承担用户认证。
// reject 由路由层注入，所有拒绝分支均停止后续 Handler。
func CORS(allowedOrigins []string, reject gin.HandlerFunc) gin.HandlerFunc {
	// 1. 复制白名单为只读查找表，避免运行期间受调用方切片修改影响。
	originSet := make(map[string]bool, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		originSet[origin] = true
	}
	return func(requestContext *gin.Context) {
		// 2. 添加缓存区分标记；无 Origin 时继续，有 Origin 时严格匹配白名单。
		request := requestContext.Request
		responseHeaders := requestContext.Writer.Header()
		addVary(responseHeaders, "Origin")
		if request.Method == http.MethodOptions {
			addVary(responseHeaders, "Access-Control-Request-Method", "Access-Control-Request-Headers")
		}
		origins := request.Header.Values("Origin")
		if len(origins) == 0 {
			requestContext.Next()
			return
		}
		if len(origins) != 1 || origins[0] == "" || origins[0] == "null" || origins[0] == "*" || !originSet[origins[0]] {
			reject(requestContext)
			requestContext.Abort()
			return
		}

		// 3. OPTIONS 预检校验目标方法和请求头，通过后返回无正文的 204。
		requestedMethods := request.Header.Values("Access-Control-Request-Method")
		if request.Method == http.MethodOptions && len(requestedMethods) > 0 {
			if len(requestedMethods) != 1 || !allowedPreflightMethod(requestedMethods[0]) || !allowedPreflightHeaders(request.Header.Values("Access-Control-Request-Headers")) {
				reject(requestContext)
				requestContext.Abort()
				return
			}
			requestContext.Header("Access-Control-Allow-Origin", origins[0])
			requestContext.Header("Access-Control-Allow-Credentials", "true")
			requestContext.Header("Access-Control-Allow-Methods", corsMethods)
			requestContext.Header("Access-Control-Allow-Headers", corsHeaders)
			requestContext.Header("Cache-Control", "no-store")
			requestContext.AbortWithStatus(http.StatusNoContent)
			return
		}

		// 4. 普通请求添加明确来源、凭证许可与可读取的请求 ID 头，再继续处理。
		requestContext.Header("Access-Control-Allow-Origin", origins[0])
		requestContext.Header("Access-Control-Allow-Credentials", "true")
		requestContext.Header("Access-Control-Expose-Headers", RequestIDHeader)
		requestContext.Next()
	}
}

// allowedPreflightMethod 只接受明确列出的、大小写准确的 HTTP 方法。
func allowedPreflightMethod(method string) bool {
	// 1. 精确比较完整方法名，不接受列表、通配符或未支持的方法。
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

// allowedPreflightHeaders 校验可跨行出现的逗号列表；未声明任何头时直接通过。
func allowedPreflightHeaders(headerLines []string) bool {
	// 1. 拆开所有列表项，逐项去掉 HTTP 列表空白并忽略头名称大小写。
	for _, line := range headerLines {
		for _, name := range strings.Split(line, ",") {
			// 2. 只接受固定名称，空项、通配符及非法名称均返回 false。
			switch strings.ToLower(strings.Trim(name, " \t")) {
			case "authorization", "content-type", "idempotency-key", "x-cloudnest-client", "x-request-id":
			default:
				return false
			}
		}
	}
	return true
}

// addVary 合并缓存，区分字段，保留既有值，并按大小写不敏感规则去重。
func addVary(headers http.Header, names ...string) {
	// 1. 收集已有字段；通配符已覆盖全部请求头，不再追加。
	seen := make(map[string]bool)
	for _, line := range headers.Values("Vary") {
		for _, name := range strings.Split(line, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name == "*" {
				return
			}
			seen[name] = true
		}
	}
	// 2. 仅追加缺失字段，不覆盖其他中间件设置的 Vary。
	for _, name := range names {
		if !seen[strings.ToLower(name)] {
			headers.Add("Vary", name)
			seen[strings.ToLower(name)] = true
		}
	}
}
