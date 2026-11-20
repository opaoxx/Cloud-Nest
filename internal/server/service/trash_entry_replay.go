package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateTrashEntryReplay 检查保存的记录是否对应当前单项回收请求。
// 调用方先完成认证、生成请求摘要并按用户范围读取记录，通过后恢复完整快照。
func ValidateTrashEntryReplay(record *domain.IdempotencyRecord, userID, entryID, expectedRevision uint64, key string, requestHash [sha256.Size]byte, now time.Time) error {
	// 1. 校验调用参数及记录归属。
	if userID == 0 || entryID == 0 || expectedRevision == 0 || !domain.ValidIdempotencyKey(key) || now.IsZero() {
		return fmt.Errorf("校验回收幂等结果需要非零用户、目标和预期版本、有效幂等键及判断时间")
	}
	if record == nil || record.ID == 0 || record.UserID != userID || record.Operation != domain.TrashEntryOperation || record.IdempotencyKey != key {
		return domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 检查保留期限，比对请求摘要。
	createdAt, expiresAt := record.CreatedAt.UTC(), record.ExpiresAt.UTC()
	if createdAt.Year() < 1000 || createdAt.Year() > 9999 || expiresAt.Year() < 1000 || expiresAt.Year() > 9999 || !expiresAt.After(createdAt) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	if !now.Before(expiresAt) {
		return domain.ErrIdempotencyRecordExpired
	}
	if record.RequestHash != requestHash {
		return domain.ErrIdempotencyRequestConflict
	}

	// 3. 核对成功记录的资源 ID 和回收后版本。
	if record.ResourceType != "ENTRY" || record.ResourceID == nil || *record.ResourceID != entryID || record.ResponseStatus != 200 ||
		expectedRevision == math.MaxUint64 || !utf8.Valid(record.ResponseBody) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	// 此处核对请求与结果的关联，完整字段由 RestoreTrashEntryData 校验。
	var body struct {
		Entry *struct {
			ID       string `json:"id"`
			Revision string `json:"revision"`
		} `json:"entry"`
	}
	if json.Unmarshal(record.ResponseBody, &body) != nil || body.Entry == nil ||
		body.Entry.ID != strconv.FormatUint(entryID, 10) || body.Entry.Revision != strconv.FormatUint(expectedRevision+1, 10) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	return nil
}
