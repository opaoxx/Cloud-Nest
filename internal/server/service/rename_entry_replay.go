package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateRenameEntryReplay 检查已读取记录是否可用于当前重命名请求。
// 调用方先完成认证并读取同范围记录，校验通过后使用既有恢复方法核对完整快照。
// 判断时间由调用方提供，到期判断采用 now >= expires_at，方法保留输入原值。
func ValidateRenameEntryReplay(record *domain.IdempotencyRecord, userID, entryID uint64, key string, requestHash [sha256.Size]byte, now time.Time) error {
	// 1. 检查调用参数及记录的用户、操作和键范围。
	if userID == 0 || entryID == 0 || !domain.ValidIdempotencyKey(key) || now.IsZero() {
		return fmt.Errorf("校验重命名幂等结果需要非零用户及目标 ID、有效幂等键和判断时间")
	}
	if record == nil || record.ID == 0 || record.UserID != userID || record.Operation != domain.RenameEntryOperation || record.IdempotencyKey != key {
		return domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 检查记录时间及保留期限。
	createdAt, expiresAt := record.CreatedAt.UTC(), record.ExpiresAt.UTC()
	if createdAt.Year() < 1000 || createdAt.Year() > 9999 || expiresAt.Year() < 1000 || expiresAt.Year() > 9999 || !expiresAt.After(createdAt) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	if !now.Before(expiresAt) {
		return domain.ErrIdempotencyRecordExpired
	}

	// 3. 比对请求摘要，识别同键参数冲突。
	if record.RequestHash != requestHash {
		return domain.ErrIdempotencyRequestConflict
	}

	// 4. 核对重命名成功元数据及正文中的目标标识。
	if record.ResourceType != "ENTRY" || record.ResourceID == nil || *record.ResourceID != entryID || record.ResponseStatus != 200 || !utf8.Valid(record.ResponseBody) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	// 这里只读取目标标识，完整公开字段由重命名快照恢复方法核对。
	var body struct {
		Entry *struct {
			ID string `json:"id"`
		} `json:"entry"`
	}
	if json.Unmarshal(record.ResponseBody, &body) != nil || body.Entry == nil || body.Entry.ID != strconv.FormatUint(entryID, 10) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	return nil
}
