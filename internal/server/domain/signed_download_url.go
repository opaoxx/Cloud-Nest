package domain

import (
	"net/url"
	"strings"
	"time"
)

// SignedDownloadURL 保存存储端短期 GET URL 及其签名截止时间。
type SignedDownloadURL struct {
	// URL 是交给客户端原样使用的签名地址。
	URL string
	// ExpiresAt 是 URL 签名的 UTC 截止时间。
	ExpiresAt time.Time
}

// IsValid 检查签名链接的公开地址与必要到期时间。
func (link SignedDownloadURL) IsValid() bool {
	return validSignedStorageURL(link.URL, link.ExpiresAt)
}

func validSignedStorageURL(rawURL string, expiresAt time.Time) bool {
	// 1. 要求 HTTP(S) 地址，拒绝片段、空白及需重新编码的原始字符。
	if expiresAt.IsZero() || (!strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://")) ||
		strings.ContainsFunc(rawURL, func(character rune) bool { return character <= 32 || character >= 127 || character == '#' }) {
		return false
	}

	// 2. 核对主机与 URL 认证信息，签名文本由调用方按原值交付。
	address, err := url.Parse(rawURL)
	return err == nil && address.Hostname() != "" && address.User == nil
}
