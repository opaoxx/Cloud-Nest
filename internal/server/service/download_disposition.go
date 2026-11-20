package service

import (
	"strings"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const downloadFilenameAlphanumeric = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// BuildDownloadDisposition 生成供下载签名绑定的 Content-Disposition 值。
// 用途决定 attachment 或 inline，文件名按目录项命名规则校验。
func BuildDownloadDisposition(purpose, rawName string) (string, error) {
	// 1. 按用途选择交付方式并校验文件名。
	var disposition string
	switch purpose {
	case domain.DownloadPurposeDownload:
		disposition = "attachment"
	case domain.DownloadPurposePreview:
		disposition = "inline"
	default:
		return "", domain.ErrInvalidDownloadPurpose
	}
	name, err := ValidateEntryName(rawName)
	if err != nil {
		return "", err
	}

	// 2. 生成安全的 ASCII 备用名。
	fallback := downloadFallbackFilename(name)

	// 3. 编码 UTF-8 文件名并组合响应头值。
	return disposition + `; filename="` + fallback + `"; filename*=UTF-8''` + encodeDownloadFilename(name), nil
}

// downloadFallbackFilename 保留安全的 ASCII 文件名，其他名称使用 download 和可用后缀。
func downloadFallbackFilename(name string) string {
	// 1. 常见 ASCII 文件名可以原样放入引号。
	if !strings.ContainsFunc(name, func(character rune) bool {
		return !strings.ContainsRune(downloadFilenameAlphanumeric+" ._-", character)
	}) {
		return name
	}

	// 2. 使用固定备用名，保留纯字母数字的末级后缀。
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		extension := name[dot+1:]
		if !strings.ContainsFunc(extension, func(character rune) bool {
			return !strings.ContainsRune(downloadFilenameAlphanumeric, character)
		}) {
			return "download." + extension
		}
	}
	return "download"
}

// encodeDownloadFilename 按 RFC 8187 编码文件名的 UTF-8 字节。
func encodeDownloadFilename(name string) string {
	// 1. 保留 attr-char 中允许直出的字节，其余字节使用百分号和两位十六进制。
	const attributeCharacters = downloadFilenameAlphanumeric + "!#$&+-.^_`|~"
	const hexDigits = "0123456789ABCDEF"
	var encoded strings.Builder
	encoded.Grow(len(name))
	for index := 0; index < len(name); index++ {
		character := name[index]
		if strings.IndexByte(attributeCharacters, character) >= 0 {
			encoded.WriteByte(character)
		} else {
			encoded.WriteByte('%')
			encoded.WriteByte(hexDigits[character>>4])
			encoded.WriteByte(hexDigits[character&15])
		}
	}
	return encoded.String()
}
