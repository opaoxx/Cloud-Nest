package service

import (
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// BuildUploadConflictName 根据原文件名和正整数编号生成一个重名候选。
// 保留末级扩展名。无法放下编号、完整扩展名和至少一个基名字符时返回 ErrEntryNameConflict。
func BuildUploadConflictName(rawName string, suffixNumber int) (string, error) {
	// 1. 校验原文件名和重名编号。
	name, err := ValidateEntryName(rawName)
	if err != nil {
		return "", err
	}
	if suffixNumber <= 0 {
		return "", domain.ErrInvalidEntryName
	}

	// 2. 分出基名与扩展名，给编号和扩展名预留长度。
	baseName, extension := name, ""
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		baseName, extension = name[:dot], name[dot:]
	}
	numberedSuffix := " (" + strconv.Itoa(suffixNumber) + ")"
	remainingUnits := maxEntryNameUTF16Units - len(numberedSuffix)
	for _, character := range extension {
		remainingUnits -= utf16.RuneLen(character)
	}

	// 3. 按完整字符截短基名，拼接并校验候选。
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
