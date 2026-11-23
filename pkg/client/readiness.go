// Package client 通过公开 HTTP 接口连接 CloudNest 服务端。
package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Readiness 检查用户指定的 CloudNest 服务是否已经就绪。
type Readiness struct{}

// Check 访问服务根地址的就绪接口，返回可在桌面窗口显示的错误。
func (*Readiness) Check(ctx context.Context, address string) error {
	// 1. 只接受服务根地址，避免把凭证或输入的其他路径发送给服务器。
	base, err := parseServiceAddress(address)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base.JoinPath("health", "ready").String(), nil)
	if err != nil {
		return errors.New("无法使用这个服务地址，请检查后重试")
	}

	// 2. 给后端的五秒依赖检查留出网络时间，只检查配置的服务本身。
	response, err := serviceHTTPClient.Do(request)
	if err != nil {
		return errors.New("无法连接服务，请检查服务地址、网络及后端是否已启动，然后重试")
	}
	defer response.Body.Close()

	// 3. 状态码与 JSON 状态须一致，避免将其他站点或代理的成功页面误报为就绪。
	const maxResponseBytes = 4096
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return errors.New("未能读完服务响应，请检查网络后重试")
	}
	var result struct {
		Status string `json:"status"`
	}
	if len(body) <= maxResponseBytes && json.Unmarshal(body, &result) == nil {
		if response.StatusCode == http.StatusOK && result.Status == "ready" {
			return nil
		}
		if response.StatusCode == http.StatusServiceUnavailable && result.Status == "not_ready" {
			return errors.New("已连接，服务尚未就绪。请检查后端的 MySQL 和 MinIO，稍后重试")
		}
	}
	return errors.New("收到的响应与 CloudNest 就绪接口不符，请核对服务地址")
}

var serviceHTTPClient = http.Client{
	Timeout: 8 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func parseServiceAddress(address string) (*url.URL, error) {
	base, err := url.Parse(strings.TrimSpace(address))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Hostname() == "" ||
		base.User != nil || (base.EscapedPath() != "" && base.EscapedPath() != "/") ||
		base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return nil, errors.New("请输入 HTTP 或 HTTPS 服务根地址，例如 http://127.0.0.1:18081")
	}
	return base, nil
}
