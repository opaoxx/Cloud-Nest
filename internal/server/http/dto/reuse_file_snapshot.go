package dto

import (
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ReuseFileData 保存秒传创建时的公开目录项，可直接编码为幂等结果 JSON。
type ReuseFileData struct {
	Entry DirectoryEntrySummary `json:"entry"`
}

// NewReuseFileData 从已写入的新文件生成稳定的公开快照。
func NewReuseFileData(result *domain.FileEntry) (*ReuseFileData, error) {
	// 1. 核对新建文件的类型、版本及展示名。
	if result == nil || result.ParentID == nil || *result.ParentID == result.ID ||
		result.EntryType != "FILE" || result.Revision != 1 || !utf8.ValidString(result.Name) {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 复用目录摘要转换，确认初始时间一致。
	entry, err := newDirectoryEntrySummary(*result)
	if err != nil || !entry.CreatedAt.Equal(entry.ModifiedAt) || !entry.CreatedAt.Equal(entry.UpdatedAt) {
		return nil, domain.ErrInvalidFileEntryData
	}
	return &ReuseFileData{Entry: *entry}, nil
}

// RestoreReuseFileData 校验并恢复已保存的秒传创建快照。
func RestoreReuseFileData(body []byte) (*ReuseFileData, error) {
	// 1. 检查唯一的 entry 对象及十项字段，保持字节数为 JSON 数字。
	fields, validFields := readEntrySnapshotFields(body, 10, "size_bytes")
	if !validFields {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 校验标识、大小和新建文件的固定状态。
	entryID, validID := parsePathID(fields["id"])
	parentID, validParent := parsePathID(fields["parent_id"])
	sizeBytes, sizeError := strconv.ParseUint(fields["size_bytes"], 10, 64)
	if !validID || !validParent || entryID == parentID || fields["name"] == "" || sizeError != nil || sizeBytes > maxJSONSafeInteger ||
		fields["entry_type"] != "FILE" || fields["revision"] != "1" || fields["status"] != "ACTIVE" {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 3. 验证三个初始时间指向同一时刻，再按既有规则转换为 UTC 微秒。
	createdAt, createError := time.Parse(time.RFC3339Nano, fields["created_at"])
	modifiedAt, modifyError := time.Parse(time.RFC3339Nano, fields["modified_at"])
	updatedAt, updateError := time.Parse(time.RFC3339Nano, fields["updated_at"])
	normalized, validTime := listResponseTime(&createdAt)
	if createError != nil || modifyError != nil || updateError != nil || !validTime ||
		!createdAt.Equal(modifiedAt) || !createdAt.Equal(updatedAt) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 4. 交付独立快照供后续同键重试返回。
	return &ReuseFileData{Entry: DirectoryEntrySummary{
		ID:         fields["id"],
		ParentID:   fields["parent_id"],
		Name:       fields["name"],
		EntryType:  fields["entry_type"],
		SizeBytes:  sizeBytes,
		Revision:   fields["revision"],
		CreatedAt:  *normalized,
		ModifiedAt: *normalized,
		UpdatedAt:  *normalized,
		Status:     fields["status"],
	}}, nil
}
