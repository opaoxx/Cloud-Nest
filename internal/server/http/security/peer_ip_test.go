package security

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDirectPeerIP(t *testing.T) {
	for _, testCase := range []struct{ name, remote, want string }{
		{name: "ipv4", remote: "192.0.2.1:1234", want: "192.0.2.1"},
		{name: "ipv6", remote: "[2001:db8::1]:65535", want: "2001:db8::1"},
		{name: "mapped ipv4", remote: "[::ffff:192.0.2.1]:1", want: "192.0.2.1"},
		{name: "loopback", remote: "127.0.0.1:32100", want: "127.0.0.1"},
		{name: "empty"},
		{name: "hostname", remote: "private-host:1234"},
		{name: "missing port", remote: "192.0.2.1"},
		{name: "unbracketed ipv6", remote: "2001:db8::1:1234"},
		{name: "zero port", remote: "192.0.2.1:0"},
		{name: "large port", remote: "192.0.2.1:65536"},
		{name: "invalid port", remote: "192.0.2.1:private-port"},
		{name: "zone", remote: "[fe80::1%private-zone]:1234"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/test", nil)
			request.RemoteAddr = testCase.remote
			request.Header.Set("X-Forwarded-For", "198.51.100.99")
			request.Header.Set("X-Real-IP", "198.51.100.98")
			request.Header.Set("Forwarded", "for=198.51.100.97")
			address, err := DirectPeerIP(request)
			if testCase.want != "" {
				if err != nil || address.String() != testCase.want {
					t.Fatalf("连接地址解析错误 address=%s error=%v", address, err)
				}
			} else if err == nil || address.IsValid() || strings.Contains(err.Error(), "private-") {
				t.Fatal("无效连接地址被接受、回退到转发头或回显输入")
			}
		})
	}
	if address, err := DirectPeerIP(nil); err == nil || address.IsValid() {
		t.Fatal("nil 请求没有明确失败")
	}
}
