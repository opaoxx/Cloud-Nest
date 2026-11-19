package service

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// maxEntryNameUTF16Units 是普通目录项展示名的 UTF-16 长度上限。
const maxEntryNameUTF16Units = 120

// reservedEntryDeviceName 匹配大小写不敏感的 Windows 设备基本名。
var reservedEntryDeviceName = regexp.MustCompile(`(?i)^(CON|PRN|AUX|NUL|COM[1-9¹²³]|LPT[1-9¹²³])$`)

// ValidateEntryName 校验普通文件或目录的展示名，成功返回去除首尾空白的名称。
// 保留名称大小写及 Unicode 写法，失败返回空字符串与固定领域错误。
func ValidateEntryName(rawName string) (string, error) {
	// 1. 检查原始文本编码和控制字符。
	if !utf8.ValidString(rawName) || strings.ContainsFunc(rawName, unicode.IsControl) {
		return "", domain.ErrInvalidEntryName
	}

	// 2. 去除首尾空白，检查空名称和末尾句点。
	name := strings.TrimSpace(rawName)
	if name == "" || strings.HasSuffix(name, ".") {
		return "", domain.ErrInvalidEntryName
	}

	// 3. 检查保留字符及 UTF-16 长度。
	utf16Length := 0
	for _, character := range name {
		if strings.ContainsRune(`<>:"/\|?*`, character) {
			return "", domain.ErrInvalidEntryName
		}
		utf16Length++
		if character > 0xFFFF {
			utf16Length++
		}
		if utf16Length > maxEntryNameUTF16Units {
			return "", domain.ErrInvalidEntryName
		}
	}

	// 4. 检查设备保留名，返回有效展示名。
	baseName, _, _ := strings.Cut(name, ".")
	baseName = strings.TrimRight(baseName, " ")
	if reservedEntryDeviceName.MatchString(baseName) || strings.EqualFold(name, "CONIN$") || strings.EqualFold(name, "CONOUT$") {
		return "", domain.ErrInvalidEntryName
	}
	return name, nil
}
