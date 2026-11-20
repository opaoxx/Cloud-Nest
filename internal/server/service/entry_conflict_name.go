package service

import (
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// BuildRestoreConflictName 根据回收项目的类型、原名和编号生成一个恢复候选名。
// 文件保留末级扩展名，目录在名称末尾追加编号。编号须为正整数。
func BuildRestoreConflictName(rawName, entryType string, suffixNumber int) (string, error) {
	// 1. 校验项目类型，再进入共用编号规则。
	switch entryType {
	case "FILE":
		return buildEntryConflictName(rawName, suffixNumber, true)
	case "DIR":
		return buildEntryConflictName(rawName, suffixNumber, false)
	default:
		return "", domain.ErrInvalidFileEntryData
	}
}

func buildEntryConflictName(rawName string, suffixNumber int, preserveExtension bool) (string, error) {
	// 1. 校验原名称和重名编号。
	name, err := ValidateEntryName(rawName)
	if err != nil {
		return "", err
	}
	if suffixNumber <= 0 {
		return "", domain.ErrInvalidEntryName
	}

	// 2. 按项目类型决定是否拆分扩展名，给编号和扩展名预留长度。
	baseName, extension := name, ""
	if dot := strings.LastIndexByte(name, '.'); preserveExtension && dot > 0 {
		baseName, extension = name[:dot], name[dot:]
	}
	numberedSuffix := " (" + strconv.Itoa(suffixNumber) + ")"
	remainingUnits := maxEntryNameUTF16Units - len(numberedSuffix)
	for _, character := range extension {
		remainingUnits -= utf16.RuneLen(character)
	}

	// 3. 按完整 Unicode 码点截短基名，拼接并校验候选。
	baseNameEnd := len(baseName)
	for byteOffset, character := range baseName {
		remainingUnits -= utf16.RuneLen(character)
		if remainingUnits < 0 {
			baseNameEnd = byteOffset
			break
		}
	}
	if baseNameEnd == 0 {
		return "", domain.ErrEntryNameConflict
	}
	return ValidateEntryName(baseName[:baseNameEnd] + numberedSuffix + extension)
}
