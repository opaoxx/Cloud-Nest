package domain

import "strings"

// ValidIdempotencyKey 校验已去除首尾 HTTP 空白的请求键，保留其大小写。
func ValidIdempotencyKey(key string) bool {
	// 1. 检查数据库容量、可见 ASCII 及单值字符规则。
	return len(key) >= 1 && len(key) <= 128 && !strings.ContainsFunc(key, func(character rune) bool {
		return character < 0x21 || character > 0x7e || character == ','
	})
}
