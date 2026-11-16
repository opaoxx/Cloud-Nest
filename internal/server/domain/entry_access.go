package domain

import "errors"

// ErrInvalidEntryAccessPurpose 表示访问用途缺少明确的 preview/open 值。
var ErrInvalidEntryAccessPurpose = errors.New("访问用途须明确为 preview 或 open")

// ValidEntryAccessPurpose 检查预览或打开使用的准确小写用途。
func ValidEntryAccessPurpose(purpose string) bool {
	// 1. 检查用途为 preview 或 open。
	return purpose == "preview" || purpose == "open"
}
