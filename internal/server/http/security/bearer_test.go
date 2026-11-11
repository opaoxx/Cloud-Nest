package security

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestBearerToken(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		values []string
		want   string
	}{
		{name: "standard", values: []string{"Bearer AbC.def-123_XY.z"}, want: "AbC.def-123_XY.z"},
		{name: "lowercase scheme", values: []string{"bearer AbC"}, want: "AbC"},
		{name: "mixed case scheme", values: []string{"bEaReR AbC"}, want: "AbC"},
		{name: "multiple separating spaces", values: []string{"Bearer   AbC"}, want: "AbC"},
		{name: "outer optional whitespace", values: []string{" \tBearer AbC\t "}, want: "AbC"},
		{name: "bearer alphabet", values: []string{"Bearer AZaz09-._~+/=="}, want: "AZaz09-._~+/=="},
		{name: "one character", values: []string{"Bearer A"}, want: "A"},
		{name: "syntactic token without jwt structure", values: []string{"Bearer ..."}, want: "..."},
		{name: "missing"},
		{name: "empty", values: []string{""}},
		{name: "whitespace only", values: []string{" \t "}},
		{name: "missing separator", values: []string{"Bearer"}},
		{name: "empty credential", values: []string{"Bearer   "}},
		{name: "basic scheme", values: []string{"Basic private-token"}},
		{name: "scheme prefix", values: []string{"BearerX private-token"}},
		{name: "tab separator", values: []string{"Bearer\tprivate-token"}},
		{name: "mixed tab separator", values: []string{"Bearer \tprivate-token"}},
		{name: "embedded space", values: []string{"Bearer private token"}},
		{name: "embedded tab", values: []string{"Bearer private\ttoken"}},
		{name: "line break", values: []string{"Bearer private\r\ntoken"}},
		{name: "nul", values: []string{"Bearer private\x00token"}},
		{name: "non ascii", values: []string{"Bearer private令牌"}},
		{name: "unicode space", values: []string{"Bearer\u00a0private-token"}},
		{name: "quoted credential", values: []string{`Bearer "private-token"`}},
		{name: "percent encoding", values: []string{"Bearer private%2Etoken"}},
		{name: "credential parameter", values: []string{"Bearer token=private-token"}},
		{name: "only padding", values: []string{"Bearer ==="}},
		{name: "leading padding", values: []string{"Bearer =private-token"}},
		{name: "embedded padding", values: []string{"Bearer private=token="}},
		{name: "comma joined", values: []string{"Bearer private-token, Bearer second-token"}},
		{name: "identical duplicates", values: []string{"Bearer private-token", "Bearer private-token"}},
		{name: "different duplicates", values: []string{"Bearer private-token", "Basic second-token"}},
		{name: "duplicate empty value", values: []string{"Bearer private-token", ""}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest("GET", "/test?access_token=private-query", nil)
			request.Header.Set("Cookie", "access_token=private-cookie")
			for _, value := range testCase.values {
				request.Header.Add("Authorization", value)
			}
			originalHeaders := request.Header.Clone()
			tokenText, err := BearerToken(request)
			if testCase.want != "" {
				if err != nil || tokenText != testCase.want {
					t.Fatal("有效头未原样提取凭证")
				}
			} else if !errors.Is(err, ErrInvalidAuthorizationHeader) || tokenText != "" {
				t.Fatal("异常头应返回固定错误与空令牌")
			}
			if err != nil && strings.Contains(err.Error(), "private-") {
				t.Fatal("错误回显了敏感输入")
			}
			if !reflect.DeepEqual(request.Header, originalHeaders) {
				t.Fatal("解析修改了请求头")
			}
		})
	}
	if tokenText, err := BearerToken(nil); tokenText != "" || !errors.Is(err, ErrInvalidAuthorizationHeader) {
		t.Fatal("nil 请求应明确失败")
	}
	if tokenText, err := BearerToken(&http.Request{}); tokenText != "" || !errors.Is(err, ErrInvalidAuthorizationHeader) {
		t.Fatal("空请求应明确失败")
	}
}

func TestBearerTokenCharacterBoundary(t *testing.T) {
	// 在凭证中间逐一放入所有字节，避免头值两端可选空白影响字符边界。
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~+/"
	for character := 0; character <= 255; character++ {
		credential := "A" + string([]byte{byte(character)}) + "Z"
		request := httptest.NewRequest("GET", "/test", nil)
		request.Header.Set("Authorization", "Bearer "+credential)
		tokenText, err := BearerToken(request)
		allowed := strings.IndexByte(alphabet, byte(character)) >= 0
		if allowed {
			if err != nil || tokenText != credential {
				t.Fatalf("有效凭证字符被拒绝 byte=%d", character)
			}
		} else if !errors.Is(err, ErrInvalidAuthorizationHeader) || tokenText != "" {
			t.Fatalf("非法凭证字符被接受 byte=%d", character)
		}
	}
}
