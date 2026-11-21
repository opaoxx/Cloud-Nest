package domain

import "time"

// SignedUploadPartURL 保存单片短期 PUT 地址与签名的实际 UTC 截止时间。
type SignedUploadPartURL struct {
	URL       string
	ExpiresAt time.Time
}
