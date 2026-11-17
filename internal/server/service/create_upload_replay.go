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

// ValidateCreateUploadReplay 检查已读取记录能否用于当前上传创建请求。
// 调用方先完成认证并读取同范围记录，通过后使用恢复方法校验完整快照。
// 到期条件为 now >= expires_at，判断时间由调用方传入，保留输入原值。
func ValidateCreateUploadReplay(record *domain.IdempotencyRecord, userID uint64, key string, requestHash [sha256.Size]byte, now time.Time) error {
	// 1. 检查调用参数和记录的用户、操作、键范围。
	if userID == 0 || !domain.ValidIdempotencyKey(key) || now.IsZero() {
		return fmt.Errorf("校验上传创建幂等结果需要非零用户 ID、有效幂等键和判断时间")
	}
	if record == nil || record.ID == 0 || record.UserID != userID || record.Operation != domain.CreateUploadOperation || record.IdempotencyKey != key {
		return domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 检查记录时间与保留期限。
	createdAt, expiresAt := record.CreatedAt.UTC(), record.ExpiresAt.UTC()
	if createdAt.Year() < 1000 || createdAt.Year() > 9999 || expiresAt.Year() < 1000 || expiresAt.Year() > 9999 || !expiresAt.After(createdAt) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	if !now.Before(expiresAt) {
		return domain.ErrIdempotencyRecordExpired
	}

	// 3. 比较请求摘要，识别同键参数冲突。
	if record.RequestHash != requestHash {
		return domain.ErrIdempotencyRequestConflict
	}

	// 4. 核对上传任务资源、成功状态和正文任务 ID。
	if record.ResourceType != "UPLOAD_TASK" || record.ResourceID == nil || *record.ResourceID == 0 || record.ResponseStatus != 201 || !utf8.Valid(record.ResponseBody) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	// 业务校验读取任务标识，完整公开字段继续由创建快照恢复方法核对。
	var body struct {
		Task *struct {
			ID string `json:"id"`
		} `json:"task"`
	}
	if json.Unmarshal(record.ResponseBody, &body) != nil || body.Task == nil || body.Task.ID != strconv.FormatUint(*record.ResourceID, 10) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	return nil
}
