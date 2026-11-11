package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadRefreshCookieSecure(t *testing.T) {
	t.Setenv("CLOUDNEST_COOKIE_SECURE", "fixture")
	if err := os.Unsetenv("CLOUDNEST_COOKIE_SECURE"); err != nil {
		t.Fatal(err)
	}
	if value, err := LoadRefreshCookieSecure(); err != nil || value {
		t.Fatal("本机默认 Cookie Secure 配置错误")
	}
	for _, testCase := range []struct {
		name, input string
		want, valid bool
	}{
		{name: "true", input: "true", want: true, valid: true},
		{name: "false", input: "false", valid: true},
		{name: "standard bool alias", input: "1", want: true, valid: true},
		{name: "explicit empty"},
		{name: "whitespace", input: " true "},
		{name: "invalid", input: "fixture-invalid-cookie-value"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("CLOUDNEST_COOKIE_SECURE", testCase.input)
			value, err := LoadRefreshCookieSecure()
			if testCase.valid {
				if err != nil || value != testCase.want {
					t.Fatal("Cookie 布尔配置读取错误")
				}
			} else if err == nil || value || strings.Contains(err.Error(), "fixture-invalid") {
				t.Fatal("无效 Cookie 配置未返回固定错误")
			}
		})
	}
}
