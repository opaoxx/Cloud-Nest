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
	payload := struct {
		Username       string `json:"username"`
		Password       string `json:"password"`
		InstallationID string `json:"installation_id"`
		DeviceName     string `json:"device_name"`
		Platform       string `json:"platform"`
		ClientType     string `json:"client_type"`
	}{
		Username: username, Password: password, InstallationID: installationID,
		DeviceName: "CloudNest Windows", Platform: "WINDOWS", ClientType: "native",
	}
	var result struct {
		Data struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
			nativeTokens
		} `json:"data"`
	}

	// 2. 发出原生请求，按状态码给出可重试的提示。
	status, err := postNativeAuth(ctx, address, "login", payload, &result)
	if err != nil {
		return NativeSession{}, err
	}
	switch status {
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
	if result.Data.User.Username != username || result.Data.AccessToken == "" || result.Data.RefreshToken == "" {
		return NativeSession{}, errors.New("收到的响应与 CloudNest 原生登录接口不符，请核对服务地址")
	}
	return NativeSession{
		Username: result.Data.User.Username, AccessToken: result.Data.AccessToken, RefreshToken: result.Data.RefreshToken,
	}, nil
}

type nativeTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// ErrRefreshRejected 表示后端已拒绝当前刷新凭证。
var ErrRefreshRejected = errors.New("保存的会话已失效，请重新登录")

// RefreshNative 轮换原生刷新凭证并返回新令牌。
func RefreshNative(ctx context.Context, address, refreshToken string) (NativeSession, error) {
	// 1. 使用原生 JSON 提交已保存的刷新凭证。
	payload := struct {
		ClientType   string `json:"client_type"`
		RefreshToken string `json:"refresh_token"`
	}{ClientType: "native", RefreshToken: refreshToken}
	var result struct {
		Data nativeTokens `json:"data"`
	}
	status, err := postNativeAuth(ctx, address, "refresh", payload, &result)
	if err != nil {
		return NativeSession{}, err
	}

	// 2. 识别已失效的凭证，其他失败由调用方保留记录并提示。
	if status == http.StatusUnauthorized {
		return NativeSession{}, ErrRefreshRejected
	}
	if status != http.StatusOK {
		return NativeSession{}, errors.New("服务暂时无法恢复会话，请重新登录或稍后重启")
	}

	// 3. 确认后端交付了新访问令牌和已轮换的刷新凭证。
	if result.Data.AccessToken == "" || result.Data.RefreshToken == "" || result.Data.RefreshToken == refreshToken {
		return NativeSession{}, errors.New("收到无效的会话恢复响应，请重新登录")
	}
	return NativeSession{AccessToken: result.Data.AccessToken, RefreshToken: result.Data.RefreshToken}, nil
}

func postNativeAuth(ctx context.Context, address, operation string, payload, result any) (int, error) {
	request, err := newNativeAuthRequest(ctx, address, operation, payload)
	if err != nil {
		return 0, err
	}
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return 0, errors.New("无法连接服务，请检查地址及后端是否已启动")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, nil
	}
	if err := readSmallJSON(response.Body, result); err != nil {
		return 0, errors.New("无法读取认证响应，请核对服务地址后重试")
	}
	return response.StatusCode, nil
}

func newNativeAuthRequest(ctx context.Context, address, operation string, payload any) (*http.Request, error) {
	base, err := parseServiceAddress(address)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("无法编码认证请求")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base.JoinPath("api", "v1", "auth", operation).String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("无法使用这个服务地址，请检查后重试")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CloudNest-Client", "native")
	return request, nil
}

func readSmallJSON(body io.Reader, result any) error {
	const maxResponseBytes = 32 * 1024
	reply, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(reply) > maxResponseBytes {
		return errors.New("响应正文超过限制")
	}
	return json.Unmarshal(reply, result)
}
