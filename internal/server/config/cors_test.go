package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestLoadAllowedOriginsDefault(t *testing.T) {
	// 1. 真正移除变量，验证未设置与显式空值是不同的情况。
	t.Setenv("CLOUDNEST_ALLOWED_ORIGINS", "temporary-value")
	if err := os.Unsetenv("CLOUDNEST_ALLOWED_ORIGINS"); err != nil {
		t.Fatal(err)
	}
	origins, err := LoadAllowedOrigins()
	if err != nil || !reflect.DeepEqual(origins, []string{"http://127.0.0.1:5173"}) {
		t.Fatalf("默认前端来源不符合预期，error=%v", err)
	}
}

func TestLoadAllowedOriginsNormalization(t *testing.T) {
	// 1. 列出常见来源、等价写法、保留差异和列表去重规则。
	testCases := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "root slash", input: "http://127.0.0.1:5173/", want: []string{"http://127.0.0.1:5173"}},
		{name: "case and HTTPS default port", input: "HTTPS://EXAMPLE.COM:443/", want: []string{"https://example.com"}},
		{name: "default ports and deduplication", input: "http://EXAMPLE.com:080,http://example.com/,https://EXAMPLE.com:443,https://example.com", want: []string{"http://example.com", "https://example.com"}},
		{name: "preserves order", input: "https://second.example, http://first.example,https://second.example", want: []string{"https://second.example", "http://first.example"}},
		{name: "distinct hosts", input: "http://localhost:5173,http://127.0.0.1:5173,http://[::1]:5173", want: []string{"http://localhost:5173", "http://127.0.0.1:5173", "http://[::1]:5173"}},
		{name: "IPv6 normalization", input: "http://[0000:0000:0000:0000:0000:0000:0000:0001]:05173/", want: []string{"http://[::1]:5173"}},
		{name: "IPv6 default HTTPS port", input: "https://[2001:0DB8::1]:443", want: []string{"https://[2001:db8::1]"}},
		{name: "punycode", input: "https://xn--fsqu00a.xn--0zwm56d", want: []string{"https://xn--fsqu00a.xn--0zwm56d"}},
		{name: "domain final dot", input: "https://Example.com.", want: []string{"https://example.com."}},
		{name: "lowest port", input: "http://localhost:1", want: []string{"http://localhost:1"}},
		{name: "highest port", input: "http://localhost:65535", want: []string{"http://localhost:65535"}},
		{name: "frontend port independent of backend reservations", input: "http://localhost:8080", want: []string{"http://localhost:8080"}},
		{name: "entry surrounding whitespace", input: " \thttp://localhost:5173/ ,\nhttps://app.example.com\r\n", want: []string{"http://localhost:5173", "https://app.example.com"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("CLOUDNEST_ALLOWED_ORIGINS", testCase.input)
			origins, err := LoadAllowedOrigins()
			if err != nil || !reflect.DeepEqual(origins, testCase.want) {
				t.Fatalf("origins=%v，error=%v，预期=%v", origins, err, testCase.want)
			}
		})
	}
}

func TestLoadAllowedOriginsInvalid(t *testing.T) {
	// 1. 错误列表不能返回部分白名单，错误也不能回显误填的凭证。
	testCases := []struct {
		name      string
		input     string
		wantIndex string
	}{
		{name: "explicit empty"},
		{name: "blank", input: " \t"},
		{name: "trailing comma", input: "http://localhost:5173,", wantIndex: "第 2 项"},
		{name: "leading comma", input: ",http://localhost:5173"},
		{name: "middle empty", input: "http://localhost:5173,,https://example.com", wantIndex: "第 2 项"},
		{name: "wildcard", input: "*"},
		{name: "subdomain wildcard", input: "https://*.example.com"},
		{name: "opaque origin", input: "null"},
		{name: "missing scheme", input: "localhost:5173"},
		{name: "unsupported scheme", input: "file:///F:/CloudNest"},
		{name: "websocket scheme", input: "ws://localhost:5173"},
		{name: "missing host", input: "http://:5173"},
		{name: "credentials", input: "http://user:private-origin-secret@localhost:5173"},
		{name: "parse error with credentials", input: "http://user:private-origin-secret@localhost:bad"},
		{name: "query", input: "https://example.com?token=private-origin-secret"},
		{name: "empty query marker", input: "https://example.com?"},
		{name: "fragment", input: "https://example.com#private-origin-secret"},
		{name: "business path", input: "http://localhost:5173/app"},
		{name: "encoded root", input: "http://localhost:5173/%2F"},
		{name: "internal whitespace", input: "http://local host:5173"},
		{name: "space separated list", input: "http://localhost:5173 https://example.com"},
		{name: "empty port", input: "http://localhost:"},
		{name: "zero port", input: "http://localhost:0"},
		{name: "port overflow", input: "http://localhost:65536"},
		{name: "negative port", input: "http://localhost:-1"},
		{name: "underscore host", input: "http://local_host:5173"},
		{name: "leading dash", input: "https://-example.com"},
		{name: "adjacent dots", input: "https://example..com"},
		{name: "long label", input: "https://" + strings.Repeat("a", 64) + ".example"},
		{name: "long domain", input: "https://" + strings.Repeat(strings.Repeat("a", 63)+".", 4) + "example"},
		{name: "unencoded internationalized host", input: "https://例子.测试"},
		{name: "short IPv4", input: "http://127.1:5173"},
		{name: "hex IPv4", input: "http://0x7f000001:5173"},
		{name: "IPv4 leading zero", input: "http://127.00.0.1:5173"},
		{name: "invalid IPv4", input: "http://999.0.0.1:5173"},
		{name: "numeric domain ending", input: "https://example.123"},
		{name: "hex domain ending", input: "https://example.0xabc"},
		{name: "unbracketed IPv6", input: "http://::1:5173"},
		{name: "bracketed IPv4", input: "http://[127.0.0.1]:5173"},
		{name: "IPv6 zone", input: "http://[fe80::1%25eth0]:5173"},
		{name: "mapped IPv6", input: "http://[::ffff:127.0.0.1]:5173"},
		{name: "invalid second item", input: "http://localhost:5173,private-origin-secret", wantIndex: "第 2 项"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("CLOUDNEST_ALLOWED_ORIGINS", testCase.input)
			origins, err := LoadAllowedOrigins()
			if err == nil || origins != nil {
				t.Fatal("无效配置应返回 nil 列表与错误")
			}
			wantIndex := testCase.wantIndex
			if wantIndex == "" {
				wantIndex = "第 1 项"
			}
			if !strings.Contains(err.Error(), "CLOUDNEST_ALLOWED_ORIGINS") || !strings.Contains(err.Error(), wantIndex) || strings.Contains(err.Error(), "private-origin-secret") {
				t.Fatal("错误未定位变量及条目，或回显了原始输入")
			}
		})
	}
}
