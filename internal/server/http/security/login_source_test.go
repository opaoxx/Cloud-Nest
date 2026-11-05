package security

import (
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestLoginSourcePolicyRules(t *testing.T) {
	// 1. 单独测试策略，不能依赖外层 CORS 恰好替它挡住非法来源。
	policy := NewLoginSourcePolicy([]string{"http://127.0.0.1:5173", "http://localhost:5173"})
	for _, testCase := range []struct {
		name, clientType string
		clients, origins []string
		want             bool
	}{
		{name: "web allowed", clientType: "web", clients: []string{"web"}, origins: []string{"http://127.0.0.1:5173"}, want: true},
		{name: "web second allowed", clientType: "web", clients: []string{"web"}, origins: []string{"http://localhost:5173"}, want: true},
		{name: "native allowed", clientType: "native", clients: []string{"native"}, want: true},
		{name: "missing client type", clients: []string{"web"}, origins: []string{"http://127.0.0.1:5173"}},
		{name: "unknown client type", clientType: "private-mode", clients: []string{"private-mode"}},
		{name: "uppercase client type", clientType: "WEB", clients: []string{"WEB"}},
		{name: "missing web marker", clientType: "web", origins: []string{"http://127.0.0.1:5173"}},
		{name: "missing native marker", clientType: "native"},
		{name: "empty marker", clientType: "native", clients: []string{""}},
		{name: "uppercase marker", clientType: "web", clients: []string{"WEB"}, origins: []string{"http://127.0.0.1:5173"}},
		{name: "whitespace marker", clientType: "native", clients: []string{" native "}},
		{name: "duplicate marker", clientType: "native", clients: []string{"native", "native"}},
		{name: "joined marker", clientType: "native", clients: []string{"native, native"}},
		{name: "web body native marker", clientType: "web", clients: []string{"native"}, origins: []string{"http://127.0.0.1:5173"}},
		{name: "native body web marker", clientType: "native", clients: []string{"web"}},
		{name: "web missing origin", clientType: "web", clients: []string{"web"}},
		{name: "web empty origin", clientType: "web", clients: []string{"web"}, origins: []string{""}},
		{name: "web null origin", clientType: "web", clients: []string{"web"}, origins: []string{"null"}},
		{name: "web wildcard origin", clientType: "web", clients: []string{"web"}, origins: []string{"*"}},
		{name: "web unknown origin", clientType: "web", clients: []string{"web"}, origins: []string{"https://private-origin.example"}},
		{name: "web duplicate origin", clientType: "web", clients: []string{"web"}, origins: []string{"http://127.0.0.1:5173", "http://127.0.0.1:5173"}},
		{name: "web origin list", clientType: "web", clients: []string{"web"}, origins: []string{"http://127.0.0.1:5173, http://localhost:5173"}},
		{name: "web suffix attack", clientType: "web", clients: []string{"web"}, origins: []string{"http://localhost:5173.private-origin.example"}},
		{name: "web slash not canonical", clientType: "web", clients: []string{"web"}, origins: []string{"http://127.0.0.1:5173/"}},
		{name: "web scheme differs", clientType: "web", clients: []string{"web"}, origins: []string{"https://127.0.0.1:5173"}},
		{name: "native with allowed origin", clientType: "native", clients: []string{"native"}, origins: []string{"http://127.0.0.1:5173"}},
		{name: "native empty origin", clientType: "native", clients: []string{"native"}, origins: []string{""}},
		{name: "native null origin", clientType: "native", clients: []string{"native"}, origins: []string{"null"}},
		{name: "native unknown origin", clientType: "native", clients: []string{"native"}, origins: []string{"https://private-origin.example"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/test", nil)
			for _, client := range testCase.clients {
				request.Header.Add(ClientHeader, client)
			}
			for _, origin := range testCase.origins {
				request.Header.Add("Origin", origin)
			}
			// Referer、Cookie 和 UA 不得让缺失的来源或声明自动补全。
			request.Header.Set("Referer", "http://127.0.0.1:5173/")
			request.Header.Set("User-Agent", "private-client")
			request.Header.Set("Cookie", "session=private-cookie")
			before := request.Header.Clone()
			if policy.Allows(request, testCase.clientType) != testCase.want {
				t.Fatal("来源判断不符合规则")
			}
			if !reflect.DeepEqual(before, request.Header) {
				t.Fatal("来源校验修改了请求头")
			}
		})
	}
}

func TestLoginSourcePolicyCopiesConfiguration(t *testing.T) {
	// 1. 原配置切片修改不能热更新既有策略；空白名单对 Web 保持拒绝。
	origins := []string{"http://127.0.0.1:5173"}
	policy := NewLoginSourcePolicy(origins)
	origins[0] = "https://private-mutated.example"
	request := httptest.NewRequest("POST", "/test", nil)
	request.Header.Set(ClientHeader, "web")
	request.Header.Set("Origin", "http://127.0.0.1:5173")
	if !policy.Allows(request, "web") || NewLoginSourcePolicy(nil).Allows(request, "web") {
		t.Fatal("白名单复制或空列表规则错误")
	}
	request.Header.Set("Origin", origins[0])
	if policy.Allows(request, "web") {
		t.Fatal("原切片修改影响了既有策略")
	}
	if policy.Allows(nil, "web") {
		t.Fatal("nil 请求被允许")
	}
}
