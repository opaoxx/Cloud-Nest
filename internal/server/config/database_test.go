package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadDatabaseConfigDefaults(t *testing.T) {
	// 1. 临时移除全部数据库配置，确保验证真正未设置变量的情况。
	for _, name := range []string{"CLOUDNEST_DB_ADDRESS", "CLOUDNEST_DB_NAME", "CLOUDNEST_DB_USER", "CLOUDNEST_DB_PASSWORD"} {
		t.Setenv(name, "temporary-value")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}

	// 2. 检查全部默认字段，不输出包含密码的配置对象。
	databaseConfig, err := LoadDatabaseConfig()
	wantConfig := DatabaseConfig{Address: "127.0.0.1:13309", DatabaseName: "cloudnest", Username: "root", Password: "root"}
	if err != nil || databaseConfig != wantConfig {
		t.Fatalf("默认配置不符合预期，error=%v", err)
	}
}

func TestLoadDatabaseConfigOverrides(t *testing.T) {
	// 1. 覆盖各项配置，检查地址规范化、凭证保真和端口边界。
	testCases := []struct {
		name        string
		address     string
		wantAddress string
		password    string
	}{
		{name: "custom IPv4", address: "127.0.0.1:13310", wantAddress: "127.0.0.1:13310", password: "custom-password"},
		{name: "hostname and special password", address: "localhost:013309", wantAddress: "localhost:13309", password: "  p@ss:/?#&= 密码  "},
		{name: "IPv6 and empty password", address: "[::1]:13309", wantAddress: "[::1]:13309"},
		{name: "lowest port", address: "127.0.0.1:1", wantAddress: "127.0.0.1:1"},
		{name: "highest port", address: "127.0.0.1:65535", wantAddress: "127.0.0.1:65535"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("CLOUDNEST_DB_ADDRESS", testCase.address)
			t.Setenv("CLOUDNEST_DB_NAME", "cloudnest_test")
			t.Setenv("CLOUDNEST_DB_USER", " custom-user ")
			t.Setenv("CLOUDNEST_DB_PASSWORD", testCase.password)

			// 2. 比较返回字段，用户名及密码必须保持调用方提供的原值。
			databaseConfig, err := LoadDatabaseConfig()
			wantConfig := DatabaseConfig{Address: testCase.wantAddress, DatabaseName: "cloudnest_test", Username: " custom-user ", Password: testCase.password}
			if err != nil || databaseConfig != wantConfig {
				t.Fatalf("覆盖配置不符合预期，error=%v", err)
			}
		})
	}
}

func TestLoadDatabaseConfigInvalid(t *testing.T) {
	// 1. 列出无效地址、端口和必填字段，检查错误定位及密码不外泄。
	testCases := []struct {
		name       string
		variable   string
		value      string
		wantReason string
	}{
		{name: "empty address", variable: "CLOUDNEST_DB_ADDRESS", wantReason: "host:port"},
		{name: "missing port", variable: "CLOUDNEST_DB_ADDRESS", value: "127.0.0.1", wantReason: "host:port"},
		{name: "missing host", variable: "CLOUDNEST_DB_ADDRESS", value: ":13309", wantReason: "指定主机"},
		{name: "empty port", variable: "CLOUDNEST_DB_ADDRESS", value: "localhost:", wantReason: "1～65535"},
		{name: "named port", variable: "CLOUDNEST_DB_ADDRESS", value: "localhost:mysql", wantReason: "1～65535"},
		{name: "zero port", variable: "CLOUDNEST_DB_ADDRESS", value: "localhost:0", wantReason: "1～65535"},
		{name: "negative port", variable: "CLOUDNEST_DB_ADDRESS", value: "localhost:-1", wantReason: "1～65535"},
		{name: "port overflow", variable: "CLOUDNEST_DB_ADDRESS", value: "localhost:65536", wantReason: "1～65535"},
		{name: "IPv6 missing brackets", variable: "CLOUDNEST_DB_ADDRESS", value: "::1:13309", wantReason: "方括号"},
		{name: "surrounding whitespace", variable: "CLOUDNEST_DB_ADDRESS", value: " localhost:13309 ", wantReason: "空白"},
		{name: "host whitespace", variable: "CLOUDNEST_DB_ADDRESS", value: "local\thost:13309", wantReason: "空白"},
		{name: "empty database", variable: "CLOUDNEST_DB_NAME", wantReason: "不能为空"},
		{name: "database whitespace", variable: "CLOUDNEST_DB_NAME", value: " cloudnest ", wantReason: "首尾空白"},
		{name: "empty username", variable: "CLOUDNEST_DB_USER", wantReason: "不能为空"},
		{name: "blank username", variable: "CLOUDNEST_DB_USER", value: " \t", wantReason: "全空白"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("CLOUDNEST_DB_ADDRESS", "127.0.0.1:13309")
			t.Setenv("CLOUDNEST_DB_NAME", "cloudnest")
			t.Setenv("CLOUDNEST_DB_USER", "root")
			t.Setenv("CLOUDNEST_DB_PASSWORD", "test-password-must-stay-private")
			t.Setenv(testCase.variable, testCase.value)

			// 2. 无效输入返回零值，错误说明配置名和原因，不携带密码。
			databaseConfig, err := LoadDatabaseConfig()
			if err == nil {
				t.Fatal("预期配置校验失败")
			}
			if databaseConfig != (DatabaseConfig{}) || !strings.Contains(err.Error(), testCase.variable) || !strings.Contains(err.Error(), testCase.wantReason) {
				t.Fatal("错误结果未满足配置名、原因或零值约定")
			}
			if strings.Contains(err.Error(), "test-password-must-stay-private") {
				t.Fatal("错误信息泄漏密码")
			}
		})
	}
}
