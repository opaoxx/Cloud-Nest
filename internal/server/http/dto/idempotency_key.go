package dto

import (
	"net/http"
	"strings"
)

// ParseIdempotencyKey 解析单个业务幂等请求头，长度与现有 ASCII VARCHAR(128) 对齐。
// 键按大小写区分，首尾 HTTP 空白去除，失败返回空字符串及固定字段错误。
func ParseIdempotencyKey(request *http.Request) (string, *FieldError) {
	// 1. 读取请求头，确认恰好提供一个幂等键。
	fieldError := &FieldError{Field: "Idempotency-Key", Message: "幂等键须为单个 1～128 字节的可见 ASCII 文本，不能含逗号或空白"}
	if request == nil {
		return "", fieldError
	}
	values := request.Header.Values("Idempotency-Key")
	if len(values) != 1 {
		return "", fieldError
	}

	// 2. 去除首尾 HTTP 空白，检查长度和字符范围。
	key := strings.Trim(values[0], " \t")
	if len(key) < 1 || len(key) > 128 || strings.ContainsFunc(key, func(character rune) bool {
		return character < 0x21 || character > 0x7e || character == ','
	}) {
		return "", fieldError
	}

	// 3. 返回保留大小写的键文本。
	return key, nil
}
