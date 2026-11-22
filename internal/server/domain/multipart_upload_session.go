package domain

import (
	"errors"
	"strings"
	"time"
)

// MaxMultipartUploadSessionsPageSize 是存储支持的会话单页上限。
const MaxMultipartUploadSessionsPageSize = 1000

// ErrMultipartUploadSessionMarkerMissing 表示续扫游标指向的原会话已消失。
var ErrMultipartUploadSessionMarkerMissing = errors.New("分片会话分页游标已失效")

// MultipartUploadSession 记录存储端仍可列出的会话及其原对象定位。
type MultipartUploadSession struct {
	ObjectKey   string
	UploadID    string
	InitiatedAt time.Time
}

// MultipartUploadSessionPage 是同一应用对象键下的一页会话。
type MultipartUploadSessionPage struct {
	Sessions []MultipartUploadSession
	// NextUploadID 为空表示末页，否则用于继续同一对象的扫描。
	NextUploadID string
}

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
