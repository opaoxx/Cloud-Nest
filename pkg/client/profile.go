package client

import (
	"context"
	"errors"
	"net/http"
)

// ErrAccessTokenExpired 对应受保护接口的 401 / TOKEN_EXPIRED。
var ErrAccessTokenExpired = errors.New("访问令牌已过期")

// ReadCurrentUsername 从本人资料接口读取已验证的账号名。
func ReadCurrentUsername(ctx context.Context, address, accessToken string) (string, error) {
	// 1. 使用 Go 客户端发送带 Bearer 令牌的本人资料请求。
	base, err := parseServiceAddress(address)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.JoinPath("api", "v1", "me").String(), nil)
	if err != nil {
		return "", errors.New("无法构造账号资料请求")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return "", errors.New("无法连接服务，请稍后重试")
	}
	defer response.Body.Close()

	// 2. 只有明确的过期错误会进入刷新，其他非成功响应按读取失败处理。
	if response.StatusCode == http.StatusUnauthorized {
		var failure struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if readSmallJSON(response.Body, &failure) == nil && failure.Error.Code == "TOKEN_EXPIRED" {
			return "", ErrAccessTokenExpired
		}
	}
	if response.StatusCode != http.StatusOK {
		return "", errors.New("服务未能返回账号资料")
	}

	// 3. 读取账号名，页面据此展示认证读取的结果。
	var result struct {
		Data struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"data"`
	}
	if readSmallJSON(response.Body, &result) != nil || result.Data.User.Username == "" {
		return "", errors.New("服务返回的账号资料格式有误")
	}
	return result.Data.User.Username, nil
}
