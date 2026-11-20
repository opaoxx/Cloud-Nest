package domain

import "regexp"

// MaxDownloadMediaTypeBytes 对应已保存 MIME 的 ASCII 列长度及公开契约上限。
const MaxDownloadMediaTypeBytes = 127

// downloadMediaTypePattern 与 OpenAPI 的 MIME 基础类型格式一致。
var downloadMediaTypePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9!#$&^_.+-]*/[a-z0-9][a-z0-9!#$&^_.+-]*$`)

// ValidDownloadMediaType 检查下载响应使用的无参数小写媒体类型。
func ValidDownloadMediaType(contentType string) bool {
	// 1. 同时核对公开长度与类型格式。
	return len(contentType) <= MaxDownloadMediaTypeBytes && downloadMediaTypePattern.MatchString(contentType)
}
