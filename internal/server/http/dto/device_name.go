package dto

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// validDeviceName 检查设备展示名称的共用输入规则，保留调用方原文。
func validDeviceName(name string) bool {
	// 1. 检查 UTF-8、字符长度、非空白及控制字符规则。
	return utf8.ValidString(name) && utf8.RuneCountInString(name) <= 100 &&
		strings.TrimSpace(name) != "" && !strings.ContainsFunc(name, unicode.IsControl)
}
