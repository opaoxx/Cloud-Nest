package service

import (
	"strings"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// BuildEntryNameMetadata 为普通目录项的新名称计算扩展名与文件分类。
// 后缀按最后一个非首位英文句点提取，超过 VARCHAR(32) 字符容量时返回空提示。
// 名称错误沿用命名错误，未知记录类型返回目录项数据错误。
func BuildEntryNameMetadata(entryType, rawName string) (*domain.EntryNameMetadata, error) {
	// 1. 检查目录项类型，复用统一名称校验。
	if entryType != "FILE" && entryType != "DIR" {
		return nil, domain.ErrInvalidFileEntryData
	}
	name, err := ValidateEntryName(rawName)
	if err != nil {
		return nil, err
	}

	// 2. 为文件夹准备空的扩展名和分类。
	metadata := &domain.EntryNameMetadata{Name: name}
	if entryType == "DIR" {
		return metadata, nil
	}
	category := "OTHER"
	metadata.Category = &category

	// 3. 提取容量范围内的小写文件扩展名。
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return metadata, nil
	}
	extension := strings.ToLower(name[dot+1:])
	if utf8.RuneCountInString(extension) > 32 {
		return metadata, nil
	}
	metadata.Extension = &extension

	// 4. 按既有扩展名清单确定文件分类。
	switch extension {
	case "jpg", "jpeg", "png", "gif", "webp", "avif", "svg":
		category = "IMAGE"
	case "mp4", "mov", "webm", "mkv":
		category = "VIDEO"
	case "mp3", "wav", "flac", "m4a", "ogg":
		category = "AUDIO"
	case "zip", "rar", "7z", "tar", "gz":
		category = "ARCHIVE"
	case "pdf", "md", "txt", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "csv", "json":
		category = "DOCUMENT"
	}
	return metadata, nil
}
