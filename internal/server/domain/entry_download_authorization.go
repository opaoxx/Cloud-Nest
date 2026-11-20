package domain

import (
	"crypto/sha256"
	"errors"
)

// ErrInvalidDownloadAuthorizationData 表示内部授权结果不能转换为公开响应。
var ErrInvalidDownloadAuthorizationData = errors.New("下载授权数据异常")

// EntryDownloadAuthorization 保存同一次授权中的短期链接和公开内容事实。
type EntryDownloadAuthorization struct {
	// Link 保存签名 URL 与截止时间。
	Link SignedDownloadURL
	// ObjectID 和 Revision 标识签名绑定的对象及目录项版本。
	ObjectID uint64
	Revision uint64
	// SizeBytes 是完成核对的对象大小。
	SizeBytes uint64
	// VerifiedSHA256 是对象已验证的内容摘要。
	VerifiedSHA256 [sha256.Size]byte
	// MIMEType 和 Disposition 是随链接签名绑定的响应头值。
	MIMEType    string
	Disposition string
}
