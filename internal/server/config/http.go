// Package config 负责读取并校验服务端配置。
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

const defaultHTTPAddress = "127.0.0.1:18081"

// LoadHTTPAddress 读取 CLOUDNEST_HTTP_ADDR，返回校验后的 host:port。
// 未设置或值为空时使用项目默认地址，格式或端口无效时返回错误。
func LoadHTTPAddress() (string, error) {
	// 1. 读取环境变量，未配置时使用项目默认监听地址。
	listenAddress := os.Getenv("CLOUDNEST_HTTP_ADDR")
	if listenAddress == "" {
		listenAddress = defaultHTTPAddress
	}

	// 2. 拆分主机与端口，检查 host:port 的结构。
	if listenAddress != strings.TrimSpace(listenAddress) {
		return "", fmt.Errorf("CLOUDNEST_HTTP_ADDR 不能包含首尾空白")
	}
	host, portText, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return "", fmt.Errorf("CLOUDNEST_HTTP_ADDR 应为 host:port，IPv6 地址需要方括号: %w", err)
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return "", fmt.Errorf("CLOUDNEST_HTTP_ADDR 的主机名称不能包含空白")
	}

	// 3. 检查端口是 1～65535 范围内的数字。
	portNumber, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || portNumber == 0 {
		return "", fmt.Errorf("CLOUDNEST_HTTP_ADDR 的端口必须是 1～65535 的数字，当前为 %q", portText)
	}

	// 4. 拒绝项目约定保留给其他应用的端口。
	if (portNumber >= 8080 && portNumber <= 8090) || portNumber == 18080 {
		return "", fmt.Errorf("CLOUDNEST_HTTP_ADDR 的端口 %d 已由项目约定保留给其他应用", portNumber)
	}

	// 5. 返回规范化后的监听地址，交给启动入口使用。
	return net.JoinHostPort(host, strconv.FormatUint(portNumber, 10)), nil
}
