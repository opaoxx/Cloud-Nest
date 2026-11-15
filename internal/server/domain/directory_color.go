package domain

import (
	"errors"
	"strings"
)

// ErrInvalidDirectoryColorKey 表示颜色键超过容量或包含非可打印 ASCII 字符。
var ErrInvalidDirectoryColorKey = errors.New("颜色键须为 null 或不超过 16 字节的可打印 ASCII 文本")

// ValidDirectoryColorKey 检查可空颜色键，保留空字符串、大小写和空格。
func ValidDirectoryColorKey(colorKey *string) bool {
	// 1. 省略或 null 使用默认配色语义。
	if colorKey == nil {
		return true
	}
	// 2. 检查 ASCII 容量及可打印字符范围。
	return len(*colorKey) <= 16 && !strings.ContainsFunc(*colorKey, func(character rune) bool {
		return character < 0x20 || character > 0x7e
	})
}
