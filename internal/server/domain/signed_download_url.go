package domain

import "time"

// SignedDownloadURL 保存存储端短期 GET URL 及其签名截止时间。
type SignedDownloadURL struct {
	// URL 是交给客户端原样使用的签名地址。
	URL string
	// ExpiresAt 是 URL 签名的 UTC 截止时间。
	ExpiresAt time.Time
}
