package client

import (
	"context"
	"errors"
	"net/http"
)

// LogoutNative 请求后端撤销该访问令牌对应的原生会话。
func LogoutNative(ctx context.Context, address, accessToken string) error {
	// 1. 使用当前令牌和原生客户端声明发出会话撤销请求。
	request, err := newNativeAuthRequest(ctx, address, "logout", struct {
		ClientType string `json:"client_type"`
	}{ClientType: "native"})
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return errors.New("无法连接服务")
	}
	defer response.Body.Close()

	// 2. 明确过期时让调用方换取新令牌，已结束的远端会话可直接完成退出。
	if response.StatusCode == http.StatusUnauthorized {
		switch authErrorCode(response.Body) {
		case "TOKEN_EXPIRED":
			return ErrAccessTokenExpired
		case "SESSION_REVOKED":
			return nil
		}
	}
	if response.StatusCode != http.StatusOK {
		return errors.New("服务端未确认撤销会话")
	}

	// 3. 核对成功回执中的 revoked 字段。
	var result struct {
		Data struct {
			Revoked bool `json:"revoked"`
		} `json:"data"`
	}
	if readSmallJSON(response.Body, &result) != nil || !result.Data.Revoked {
		return errors.New("服务返回的退出回执有误")
	}
	return nil
}
