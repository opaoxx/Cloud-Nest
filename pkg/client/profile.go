package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
)

// ErrAccessTokenExpired 对应受保护接口的 401 / TOKEN_EXPIRED。
var ErrAccessTokenExpired = errors.New("访问令牌已过期")

// ReadCurrentUsername 从本人资料接口读取已验证的账号名。
func ReadCurrentUsername(ctx context.Context, address, accessToken string) (string, error) {
	profile, err := ReadCurrentProfile(ctx, address, accessToken)
	return profile.Username, err
}

// CurrentProfile 保存桌面端读取根目录所需的账号资料。
type CurrentProfile struct {
	Username    string
	RootEntryID string
}

// ReadCurrentProfile 读取本人账号名和后端分配的根目录 ID。
func ReadCurrentProfile(ctx context.Context, address, accessToken string) (CurrentProfile, error) {
	// 1. 从本人资料端点取回账号和根目录标识。
	var result struct {
		Data struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
			RootEntryID string `json:"root_entry_id"`
		} `json:"data"`
	}
	if err := getAuthenticatedJSON(ctx, address, accessToken, "me", nil, &result); err != nil {
		return CurrentProfile{}, err
	}

	// 2. 保留字符串 ID，供目录读取校验和提交。
	if result.Data.User.Username == "" {
		return CurrentProfile{}, errors.New("服务返回的账号资料格式有误")
	}
	return CurrentProfile{Username: result.Data.User.Username, RootEntryID: result.Data.RootEntryID}, nil
}

func getAuthenticatedJSON(ctx context.Context, address, accessToken, resource string, query url.Values, result any) error {
	// 1. 在配置的服务上发出带 Bearer 令牌的只读请求。
	base, err := parseServiceAddress(address)
	if err != nil {
		return err
	}
	endpoint := base.JoinPath("api", "v1", resource)
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return errors.New("无法构造读取请求")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return errors.New("无法连接服务，请稍后重试")
	}
	defer response.Body.Close()

	// 2. 只有明确的过期错误会进入刷新，其他非成功响应按读取失败处理。
	if response.StatusCode == http.StatusUnauthorized && authErrorCode(response.Body) == "TOKEN_EXPIRED" {
		return ErrAccessTokenExpired
	}
	if response.StatusCode != http.StatusOK {
		return errors.New("服务未能返回请求的资料")
	}

	// 3. 为一页长文件名和条目元数据留出空间，并限制最大正文。
	if err := readBoundedJSON(response.Body, 256*1024, result); err != nil {
		return errors.New("服务返回的资料格式有误")
	}
	return nil
}

func authErrorCode(body io.Reader) string {
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if readSmallJSON(body, &failure) != nil {
		return ""
	}
	return failure.Error.Code
}
