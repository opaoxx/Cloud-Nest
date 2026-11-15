package dto

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestParseIdempotencyKeyResults(t *testing.T) {
	for _, testCase := range []struct{ name, header, value, want string }{
		{name: "uuid", value: "28ee758a-0727-42de-ae7d-3bb33155e148", want: "28ee758a-0727-42de-ae7d-3bb33155e148"},
		{name: "uppercase uuid", value: "28EE758A-0727-42DE-AE7D-3BB33155E148", want: "28EE758A-0727-42DE-AE7D-3BB33155E148"},
		{name: "opaque identifier", value: "Request-007", want: "Request-007"},
		{name: "minimum length", value: "0", want: "0"},
		{name: "maximum length", value: strings.Repeat("A", 128), want: strings.Repeat("A", 128)},
		{name: "http whitespace", value: " \t Request-007\t ", want: "Request-007"},
		{name: "padding outside length", value: strings.Repeat(" ", 1024) + strings.Repeat("A", 128) + "\t", want: strings.Repeat("A", 128)},
		{name: "lowercase header name", header: "idempotency-key", value: "Request-007", want: "Request-007"},
		{name: "mixed header name", header: "iDeMpOtEnCy-KeY", value: "Request-007", want: "Request-007"},
		{name: "base64 punctuation", value: "azAZ09-._~+/==", want: "azAZ09-._~+/=="},
		{name: "percent text preserved", value: "request%2F001+next", want: "request%2F001+next"},
		{name: "other visible punctuation", value: `A"b\c:!#$%&'()*;<=>?@[]^_{|}`, want: `A"b\c:!#$%&'()*;<=>?@[]^_{|}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/", nil)
			header := testCase.header
			if header == "" {
				header = "Idempotency-Key"
			}
			request.Header.Set(header, testCase.value)
			before := request.Header.Clone()
			key, fieldError := ParseIdempotencyKey(request)
			if fieldError != nil || key != testCase.want || !reflect.DeepEqual(request.Header, before) {
				t.Fatal("幂等键原文、空白处理或头字段保持错误", fieldError)
			}
			request.Header.Set(header, "changed")
			if key != testCase.want {
				t.Fatal("修改头字段影响已返回的键")
			}
		})
	}
}

func TestParseIdempotencyKeyFailures(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		values []string
	}{
		{name: "absent"},
		{name: "empty", values: []string{""}},
		{name: "blank", values: []string{" \t "}},
		{name: "overflow", values: []string{strings.Repeat("a", 129)}},
		{name: "duplicate", values: []string{"first", "second"}},
		{name: "identical duplicate", values: []string{"same", "same"}},
		{name: "duplicate empty", values: []string{"first", ""}},
		{name: "comma joined", values: []string{"first,second"}},
		{name: "comma joined with space", values: []string{"first, second"}},
		{name: "quoted comma", values: []string{`"first,second"`}},
		{name: "internal space", values: []string{"first second"}},
		{name: "internal tab", values: []string{"first\tsecond"}},
		{name: "carriage return", values: []string{"private-key\r"}},
		{name: "newline", values: []string{"private-key\n"}},
		{name: "zero byte", values: []string{"private-key\x00"}},
		{name: "unicode", values: []string{"请求编号"}},
		{name: "unicode whitespace", values: []string{"\u00a0first"}},
		{name: "invalid utf8", values: []string{"private-key\xff"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/", nil)
			for _, value := range testCase.values {
				request.Header.Add("Idempotency-Key", value)
			}
			key, fieldError := ParseIdempotencyKey(request)
			if key != "" || fieldError == nil || fieldError.Field != "Idempotency-Key" || fieldError.Message != "幂等键须为单个 1～128 字节的可见 ASCII 文本，不能含逗号或空白" {
				t.Fatal("非法幂等键未返回空结果和固定字段错误", fieldError)
			}
		})
	}
	for character := rune(0); character < 128; character++ {
		t.Run(fmt.Sprintf("ASCII U+%04X", character), func(t *testing.T) {
			value := "a" + string(character) + "z"
			request := httptest.NewRequest("POST", "/", nil)
			request.Header.Set("Idempotency-Key", value)
			key, fieldError := ParseIdempotencyKey(request)
			valid := character >= 0x21 && character <= 0x7e && character != ','
			if valid {
				if fieldError != nil || key != value {
					t.Fatal("有效 ASCII 键被错误拒绝", fieldError)
				}
			} else if key != "" || fieldError == nil || fieldError.Field != "Idempotency-Key" {
				t.Fatal("控制字符、空白或逗号未拒绝", fieldError)
			}
		})
	}
}

func TestParseIdempotencyKeySourcesAndErrors(t *testing.T) {
	_, nilError := ParseIdempotencyKey(nil)
	if nilError == nil || nilError.Field != "Idempotency-Key" {
		t.Fatal("空请求未返回字段错误")
	}
	body := strings.NewReader(`{"idempotency_key":"private-body"}`)
	request := httptest.NewRequest("POST", "/?Idempotency-Key=private-query", nil)
	request.Body = io.NopCloser(body)
	request.Header.Set("Cookie", "Idempotency-Key=private-cookie")
	request.Header.Set("X-Idempotency-Key", "private-alias")
	request.Trailer = http.Header{"Idempotency-Key": []string{"private-trailer"}}
	before := body.Len()
	key, firstError := ParseIdempotencyKey(request)
	_, secondError := ParseIdempotencyKey(request)
	if key != "" || firstError == nil || secondError == nil || firstError == secondError || body.Len() != before {
		t.Fatal("错误来源被采用、正文被读取或错误对象共享")
	}
	firstError.Field, firstError.Message = "changed", "changed"
	if secondError.Field != "Idempotency-Key" || strings.Contains(secondError.Message, "changed") {
		t.Fatal("修改错误对象影响其他结果")
	}
	request.Header.Set("Idempotency-Key", "Request-007")
	if key, fieldError := ParseIdempotencyKey(request); fieldError != nil || key != "Request-007" || body.Len() != before {
		t.Fatal("头字段解析依赖了其他请求来源", fieldError)
	}
}

func TestParseIdempotencyKeyConcurrent(t *testing.T) {
	request := httptest.NewRequest("POST", "/", nil)
	request.Header.Set("Idempotency-Key", " \t Request-007 \t")
	var workers sync.WaitGroup
	for worker := 0; worker < 64; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				key, fieldError := ParseIdempotencyKey(request)
				if fieldError != nil || key != "Request-007" {
					t.Error("并发幂等键解析错误", fieldError)
				}
			}
		}()
	}
	workers.Wait()
}
