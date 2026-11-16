package dto

import (
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RenameEntryData 保存重命名完成时可交付且可写入幂等记录的稳定结果。
type RenameEntryData struct {
	// Entry 复用公开详情字段，保存当前事务内的目录项快照。
	Entry EntryDetailData `json:"entry"`
}

// RenameEntryResponse 关联稳定重命名结果与当前 HTTP 请求 ID。
type RenameEntryResponse struct {
	// Data 保存重命名成功的稳定数据。
	Data RenameEntryData `json:"data"`
	// RequestID 由交付层按当前请求提供。
	RequestID string `json:"request_id"`
}

// NewRenameEntryData 转换同一事务内重命名后读取的完整详情。
// 调用方在外层事务确认提交后交付响应，失败返回空结果与错误。
func NewRenameEntryData(result *domain.EntryDetail) (*RenameEntryData, error) {
	// 1. 检查普通目录项及名称文本。
	if result == nil || len(result.Path.Entries) < 2 {
		return nil, domain.ErrInvalidFileEntryData
	}
	target := result.Path.Entries[len(result.Path.Entries)-1]
	if !utf8.ValidString(target.Name) {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 复用详情转换，核对归属、类型、大小和时间。
	entry, err := NewEntryDetailData(result)
	if err != nil {
		return nil, err
	}

	// 3. 组装可保存的稳定重命名结果。
	return &RenameEntryData{Entry: *entry}, nil
}
