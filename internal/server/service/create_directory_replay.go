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

// ValidateCreateDirectoryReplay 检查已读取记录是否可用于当前创建请求。
// 调用方先完成认证并读取同范围记录，校验通过后使用既有恢复方法核对完整快照。
// 判断时间由调用方提供，到期判断采用 now >= expires_at，方法保留输入原值。
func ValidateCreateDirectoryReplay(record *domain.IdempotencyRecord, userID uint64, key string, requestHash [sha256.Size]byte, now time.Time) error {
	// 1. 检查调用参数及记录的用户、操作和键范围。
	if userID == 0 || !domain.ValidIdempotencyKey(key) || now.IsZero() {
		return fmt.Errorf("校验创建文件夹幂等结果需要非零用户 ID、有效幂等键和判断时间")
	}
	if record == nil || record.ID == 0 || record.UserID != userID || record.Operation != domain.CreateDirectoryOperation || record.IdempotencyKey != key {
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

	// 4. 核对创建成功元数据及正文中的资源标识。
	if record.ResourceType != "ENTRY" || record.ResourceID == nil || *record.ResourceID == 0 || record.ResponseStatus != 201 || !utf8.Valid(record.ResponseBody) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	// 只读取服务判断需要的标识，完整公开字段由创建快照恢复方法核对。
	var body struct {
		Entry *struct {
			ID string `json:"id"`
		} `json:"entry"`
	}
	if json.Unmarshal(record.ResponseBody, &body) != nil || body.Entry == nil || body.Entry.ID != strconv.FormatUint(*record.ResourceID, 10) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	return nil
}
