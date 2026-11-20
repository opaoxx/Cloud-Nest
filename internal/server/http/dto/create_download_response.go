package dto

import (
	"encoding/hex"
	"mime"
	"strconv"
	"strings"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CreateDownloadData 保存下载和预览授权的八个公开字段。
type CreateDownloadData struct {
	// URL 是客户端需原样使用的短期签名链接。
	URL string `json:"url"`
	// ExpiresAt 是签名的 UTC 到期时间。
	ExpiresAt time.Time `json:"expires_at"`
	// ObjectID 是完整对象 ID 的十进制字符串。
	ObjectID string `json:"object_id"`
	// Revision 是签名绑定的目录项版本字符串。
	Revision string `json:"revision"`
	// SizeBytes 是 JSON 安全整数范围内的对象大小。
	SizeBytes uint64 `json:"size_bytes"`
	// SHA256 是已验证摘要的 64 位小写十六进制文本。
	SHA256 string `json:"sha256"`
	// MIMEType 是签入链接的基础媒体类型。
	MIMEType string `json:"mime_type"`
	// Disposition 是签入链接的文件名响应头值。
	Disposition string `json:"disposition"`
}

// CreateDownloadResponse 关联公开下载授权数据与当前请求 ID。
type CreateDownloadResponse struct {
	// Data 保存已转换的公开字段。
	Data CreateDownloadData `json:"data"`
	// RequestID 由 HTTP 交付层填入。
	RequestID string `json:"request_id"`
}

// NewCreateDownloadData 将服务确认提交后的内部授权转换为公开字段。
func NewCreateDownloadData(result *domain.EntryDownloadAuthorization) (*CreateDownloadData, error) {
	// 1. 检查授权结果和公开字段边界。
	if result == nil || result.ObjectID == 0 || result.Revision == 0 || result.SizeBytes > maxJSONSafeInteger ||
		!result.Link.IsValid() || !domain.ValidDownloadMediaType(result.MIMEType) || !validDownloadDisposition(result.Disposition) {
		return nil, domain.ErrInvalidDownloadAuthorizationData
	}

	// 2. 将到期时间转换为 UTC。
	expiresAt, valid := listResponseTime(&result.Link.ExpiresAt)
	if !valid {
		return nil, domain.ErrInvalidDownloadAuthorizationData
	}

	// 3. 编码对象 ID、版本和摘要，组装公开数据。
	return &CreateDownloadData{
		URL:         result.Link.URL,
		ExpiresAt:   *expiresAt,
		ObjectID:    strconv.FormatUint(result.ObjectID, 10),
		Revision:    strconv.FormatUint(result.Revision, 10),
		SizeBytes:   result.SizeBytes,
		SHA256:      hex.EncodeToString(result.VerifiedSHA256[:]),
		MIMEType:    result.MIMEType,
		Disposition: result.Disposition,
	}, nil
}

// validDownloadDisposition 核对签名响应头的语法、文件名和可打印 ASCII 边界。
func validDownloadDisposition(value string) bool {
	// 1. 拒绝控制字符，检查允许的交付方式和文件名形状。
	if strings.ContainsFunc(value, func(character rune) bool { return character < 32 || character > 126 }) {
		return false
	}
	disposition, parameters, err := mime.ParseMediaType(value)
	return err == nil && (disposition == "attachment" || disposition == "inline") &&
		strings.HasPrefix(value, disposition+";") && parameters["filename"] != ""
}
