package config

import (
	"fmt"
	"strconv"
)

// LoadRefreshCookieSecure 读取可信部署配置，HTTP 本机开发默认使用 false。
func LoadRefreshCookieSecure() (bool, error) {
	// 1. 按环境配置解析 Secure 标记，显式空值和非法值返回错误。
	secure, err := strconv.ParseBool(envOrDefault("CLOUDNEST_COOKIE_SECURE", "false"))
	if err != nil {
		return false, fmt.Errorf("CLOUDNEST_COOKIE_SECURE 须为有效布尔值，例如 true 或 false")
	}
	return secure, nil
}
