package domain

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidDeviceName 检查设备展示名称的共用输入规则，保留调用方原文。
func ValidDeviceName(name string) bool {
	// 1. 检查 UTF-8、字符长度、非空白及控制字符规则。
	return utf8.ValidString(name) && utf8.RuneCountInString(name) <= 100 &&
		strings.TrimSpace(name) != "" && !strings.ContainsFunc(name, unicode.IsControl)
}
