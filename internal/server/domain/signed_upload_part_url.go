package domain

import "time"

// SignedUploadPartURL 保存单片短期 PUT 地址与签名的实际 UTC 截止时间。
type SignedUploadPartURL struct {
	URL       string
	ExpiresAt time.Time
}

// IsValid 检查公开链接及到期时间的格式，任务期限由签发流程核对。
func (link SignedUploadPartURL) IsValid() bool {
	return validSignedStorageURL(link.URL, link.ExpiresAt)
}
