package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadHTTPAddress(t *testing.T) {
	// 1. 列出正常写法、边界值和应被拒绝的配置。
	testCases := []struct {
		name        string
		address     string
		wantAddress string
		wantError   string
	}{
		{name: "empty uses default", wantAddress: "127.0.0.1:18081"},
		{name: "custom IPv4", address: "127.0.0.1:18082", wantAddress: "127.0.0.1:18082"},
		{name: "hostname", address: "localhost:18081", wantAddress: "localhost:18081"},
		{name: "empty host", address: ":18081", wantAddress: ":18081"},
		{name: "IPv6", address: "[::1]:18081", wantAddress: "[::1]:18081"},
		{name: "normalize port", address: "127.0.0.1:018081", wantAddress: "127.0.0.1:18081"},
		{name: "lowest port", address: "127.0.0.1:1", wantAddress: "127.0.0.1:1"},
		{name: "highest port", address: "127.0.0.1:65535", wantAddress: "127.0.0.1:65535"},
		{name: "missing port", address: "127.0.0.1", wantError: "host:port"},
		{name: "empty port", address: "127.0.0.1:", wantError: "1～65535"},
		{name: "named port", address: "127.0.0.1:http", wantError: "1～65535"},
		{name: "zero port", address: "127.0.0.1:0", wantError: "1～65535"},
		{name: "negative port", address: "127.0.0.1:-1", wantError: "1～65535"},
		{name: "port overflow", address: "127.0.0.1:65536", wantError: "1～65535"},
		{name: "IPv6 missing brackets", address: "::1:18081", wantError: "host:port"},
		{name: "whitespace only", address: "  ", wantError: "首尾空白"},
		{name: "surrounding whitespace", address: " 127.0.0.1:18081 ", wantError: "首尾空白"},
		{name: "host whitespace", address: "local host:18081", wantError: "主机名称"},
		{name: "reserved lower boundary", address: "127.0.0.1:8080", wantError: "保留"},
		{name: "reserved middle", address: "localhost:8085", wantError: "保留"},
		{name: "reserved upper boundary", address: "127.0.0.1:8090", wantError: "保留"},
		{name: "reserved stopped container", address: "127.0.0.1:18080", wantError: "保留"},
	}

	// 2. 每个子测试设置自己的环境变量，测试结束后由 Go 恢复。
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("CLOUDNEST_HTTP_ADDR", testCase.address)
			address, err := LoadHTTPAddress()

			// 3. 错误输入需要明确原因，正常输入需要返回预期地址。
			if testCase.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantError) || address != "" {
					t.Fatalf("address=%q, error=%v，预期错误包含 %q", address, err, testCase.wantError)
				}
				return
			}
			if err != nil || address != testCase.wantAddress {
				t.Fatalf("address=%q, error=%v，预期地址 %q", address, err, testCase.wantAddress)
			}
		})
	}
}

func TestLoadHTTPAddressWithoutEnvironment(t *testing.T) {
	// 1. 暂存原环境，再临时移除配置，覆盖真正未设置变量的情况。
	t.Setenv("CLOUDNEST_HTTP_ADDR", "temporary-value")
	if err := os.Unsetenv("CLOUDNEST_HTTP_ADDR"); err != nil {
		t.Fatal(err)
	}

	// 2. 读取配置，确认默认地址在未设置变量时同样生效。
	address, err := LoadHTTPAddress()
	if err != nil || address != "127.0.0.1:18081" {
		t.Fatalf("address=%q, error=%v", address, err)
	}
}
