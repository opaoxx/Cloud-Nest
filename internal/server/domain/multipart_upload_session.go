package domain

import "strings"

// ValidMultipartUploadID 检查会话 ID 是否可按原值写入 storage_upload_id。
func ValidMultipartUploadID(value string) bool {
	// 1. 核对列长度及非空值。
	if len(value) > 1024 || strings.TrimSpace(value) == "" {
		return false
	}
	// 2. 保留存储端返回的原值，拒绝不可打印字符和非 ASCII 内容。
	return !strings.ContainsFunc(value, func(character rune) bool {
		return character < 32 || character > 126
	})
}
