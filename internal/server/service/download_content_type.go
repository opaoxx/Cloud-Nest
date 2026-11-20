package service

import (
	"mime"
	"regexp"
	"strings"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const (
	// defaultDownloadContentType 用于没有可靠媒体类型的普通下载。
	defaultDownloadContentType = "application/octet-stream"
	// maxDownloadMIMEBytes 对应 file_entries.mime_type 的 ASCII 列长度。
	maxDownloadMIMEBytes = 127
	// maxTextPreviewBytes 是纯文本预览允许的最大字节数。
	maxTextPreviewBytes = 2 * 1024 * 1024
)

// downloadMediaTypePattern 与 OpenAPI 的 MIME 基础类型格式一致。
var downloadMediaTypePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]*/[a-z0-9][a-z0-9!#$&^_.+-]*$`)

// ResolveDownloadContentType 按用途和已保存的内容事实选择无参数的基础媒体类型。
// 预览仅允许已确认的栅格图片和大小符合上限的纯文本。
func ResolveDownloadContentType(purpose string, storedMIME *string, sizeBytes uint64) (string, error) {
	// 1. 校验用途，解析已保存的 MIME。
	if !domain.ValidDownloadPurpose(purpose) {
		return "", domain.ErrInvalidDownloadPurpose
	}
	contentType := normalizeDownloadMediaType(storedMIME)

	// 2. 为普通下载选择媒体类型或二进制回退值。
	if purpose == domain.DownloadPurposeDownload {
		if contentType == "" {
			return defaultDownloadContentType, nil
		}
		return contentType, nil
	}

	// 3. 核对预览允许的类型及纯文本大小上限。
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return contentType, nil
	case "text/plain":
		if sizeBytes <= maxTextPreviewBytes {
			return contentType, nil
		}
	}
	return "", domain.ErrUnsupportedPreview
}

// normalizeDownloadMediaType 解析数据库保存的 MIME，缺失或无法解析时返回空值。
func normalizeDownloadMediaType(storedMIME *string) string {
	// 1. 确认存储值在列约束内，排除控制字符和非 ASCII 文本。
	if storedMIME == nil || len(*storedMIME) > maxDownloadMIMEBytes ||
		strings.ContainsFunc(*storedMIME, func(character rune) bool { return character < 32 || character > 126 }) {
		return ""
	}

	// 2. 解析参数并返回小写基础类型，拒绝通配符和不符合契约的值。
	contentType, _, err := mime.ParseMediaType(*storedMIME)
	if err != nil || !downloadMediaTypePattern.MatchString(contentType) {
		return ""
	}
	return contentType
}
