package middleware

import (
	"net/http"
	"reflect"
	"testing"
)

func TestCORSVaryPreservesExistingFields(t *testing.T) {
	// 1. 缓存可能已有其他区分条件；合并不能覆盖、重复或破坏通配符。
	for _, testCase := range []struct {
		name              string
		initial, expected []string
	}{
		{name: "empty", expected: []string{"Origin", "Access-Control-Request-Method"}},
		{name: "existing encoding", initial: []string{"Accept-Encoding"}, expected: []string{"Accept-Encoding", "Origin", "Access-Control-Request-Method"}},
		{name: "case insensitive list", initial: []string{"Accept-Encoding, origin"}, expected: []string{"Accept-Encoding, origin", "Access-Control-Request-Method"}},
		{name: "multiple lines", initial: []string{"Origin", "Access-Control-Request-Method"}, expected: []string{"Origin", "Access-Control-Request-Method"}},
		{name: "wildcard", initial: []string{"*"}, expected: []string{"*"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			headers := make(http.Header)
			for _, value := range testCase.initial {
				headers.Add("Vary", value)
			}
			addVary(headers, "Origin", "Access-Control-Request-Method", "Origin")
			if !reflect.DeepEqual(headers.Values("Vary"), testCase.expected) {
				t.Fatalf("Vary=%v", headers.Values("Vary"))
			}
		})
	}
}
