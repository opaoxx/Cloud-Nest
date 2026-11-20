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
