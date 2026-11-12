package handler

import (
	"net/http"
	"time"
)

// RefreshCookieName 是 CloudNest 的 Web 刷新凭证 Cookie 名称。
const RefreshCookieName = "cloudnest_refresh_token"

// RefreshCookiePath 限定刷新 Cookie 发送给认证接口的路径范围。
const RefreshCookiePath = "/api/v1/auth"

// newRefreshCookie 按登录和刷新共用的属性构造 Cookie，原文和期限须已通过 DTO 检查。
func newRefreshCookie(rawToken string, expiresAt time.Time, secureCookie bool) (*http.Cookie, error) {
	// 1. 以会话 UTC 整秒期限构造 host-only 刷新 Cookie。
	refreshCookie := &http.Cookie{
		Name:     RefreshCookieName,
		Value:    rawToken,
		Path:     RefreshCookiePath,
		Expires:  expiresAt.UTC().Truncate(time.Second),
		HttpOnly: true,
		Secure:   secureCookie,
		SameSite: http.SameSiteStrictMode,
	}
	// 2. 先校验 Cookie，再交给调用方写入响应。
	if err := refreshCookie.Valid(); err != nil {
		return nil, err
	}
	return refreshCookie, nil
}
