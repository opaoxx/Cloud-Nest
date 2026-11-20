package domain

import "errors"

const (
	// DownloadPurposeDownload 请求以附件方式交付文件。
	DownloadPurposeDownload = "download"
	// DownloadPurposePreview 请求在客户端预览文件。
	DownloadPurposePreview = "preview"
)

// ErrInvalidDownloadPurpose 表示用途缺失或超出下载与预览的契约。
var ErrInvalidDownloadPurpose = errors.New("下载授权用途须明确为 download 或 preview")

// ErrUnsupportedPreview 表示已保存的内容类型或文件大小不满足首期预览规则。
var ErrUnsupportedPreview = errors.New("当前文件不支持预览，请下载后查看")

// ValidDownloadPurpose 检查是否为契约支持的准确小写用途。
func ValidDownloadPurpose(purpose string) bool {
	// 1. 允许普通下载和预览用途。
	return purpose == DownloadPurposeDownload || purpose == DownloadPurposePreview
}
