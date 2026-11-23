package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// NativeSession 保存 Go 客户端后续认证请求需要的内存会话。
type NativeSession struct {
	Username     string
	AccessToken  string
	RefreshToken string
}

// LoginWindows 向指定服务建立 Windows 原生会话。
func LoginWindows(ctx context.Context, address, installationID, username, password string) (NativeSession, error) {
	// 1. 组装原生登录请求。密码保留输入原文，设备身份来自本机安装。
	base, err := parseServiceAddress(address)
	if err != nil {
		return NativeSession{}, err
	}
	body, err := json.Marshal(struct {
		Username       string `json:"username"`
		Password       string `json:"password"`
		InstallationID string `json:"installation_id"`
		DeviceName     string `json:"device_name"`
		Platform       string `json:"platform"`
		ClientType     string `json:"client_type"`
	}{
		Username: username, Password: password, InstallationID: installationID,
		DeviceName: "CloudNest Windows", Platform: "WINDOWS", ClientType: "native",
	})
	if err != nil {
		return NativeSession{}, errors.New("无法编码登录请求")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base.JoinPath("api", "v1", "auth", "login").String(), bytes.NewReader(body))
	if err != nil {
		return NativeSession{}, errors.New("无法使用这个服务地址，请检查后重试")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CloudNest-Client", "native")

	// 2. 发出带超时的请求，不跟随重定向发送账号凭证。
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return NativeSession{}, errors.New("无法连接服务，请检查地址及后端是否已启动")
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return NativeSession{}, errors.New("用户名或密码错误，请重试")
	case http.StatusBadRequest:
		return NativeSession{}, errors.New("登录参数有误，请检查用户名和密码")
	case http.StatusForbidden:
		return NativeSession{}, errors.New("服务拒绝了本次登录，请检查账号状态及服务配置")
	case http.StatusTooManyRequests:
		return NativeSession{}, errors.New("登录尝试过于频繁，请稍后重试")
	default:
		return NativeSession{}, errors.New("服务未能完成登录，请核对地址或稍后重试")
	}

	// 3. 校验原生登录响应，同时取得访问令牌和刷新凭证。
	const maxLoginResponseBytes = 32 * 1024
	reply, err := io.ReadAll(io.LimitReader(response.Body, maxLoginResponseBytes+1))
	if err != nil || len(reply) > maxLoginResponseBytes {
		return NativeSession{}, errors.New("无法读取登录响应，请重试")
	}
	var result struct {
		Data struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(reply, &result); err != nil ||
		result.Data.User.Username != username || result.Data.AccessToken == "" || result.Data.RefreshToken == "" {
		return NativeSession{}, errors.New("收到的响应与 CloudNest 原生登录接口不符，请核对服务地址")
	}
	return NativeSession{
		Username: result.Data.User.Username, AccessToken: result.Data.AccessToken, RefreshToken: result.Data.RefreshToken,
	}, nil
}
