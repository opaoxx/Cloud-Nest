package service

import (
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateCreateUploadReplacementReplay 核对同账号的替换创建回执和原请求声明。
// 调用方先复查授权，使用声明计算 requestHash，并提供完整快照的恢复校验方法。
func ValidateCreateUploadReplacementReplay(record *domain.IdempotencyRecord, userID uint64,
	declaration UploadReplacementDeclaration, key string, requestHash [sha256.Size]byte, now time.Time,
	validateSnapshot func(body []byte, taskID, entryID, expectedRevision, sizeBytes uint64) error,
) error {
	// 1. 检查调用参数，确保可按原请求核对回执。
	if userID == 0 || !domain.ValidIdempotencyKey(key) || now.IsZero() || validateSnapshot == nil {
		return fmt.Errorf("校验内容替换回执需要非零用户 ID、有效幂等键、判断时间及快照校验方法")
	}
	if err := validateUploadReplacementDeclaration(declaration.EntryID, declaration.ExpectedRevision, declaration.SizeBytes); err != nil {
		return err
	}

	// 2. 沿用范围、期限及请求摘要的检查顺序。
	if err := validateEntryReplayRecord(record, userID, domain.CreateUploadReplacementOperation, key, requestHash, now); err != nil {
		return err
	}

	// 3. 核对 201 回执及原任务和替换绑定。
	if record.ResourceType != "UPLOAD_TASK" || record.ResourceID == nil || *record.ResourceID == 0 ||
		record.ResponseStatus != 201 || len(record.ResponseBody) == 0 {
		return domain.ErrInvalidIdempotencyRecordData
	}
	if err := validateSnapshot(record.ResponseBody, *record.ResourceID, declaration.EntryID, declaration.ExpectedRevision, declaration.SizeBytes); err != nil {
		return domain.ErrInvalidIdempotencyRecordData
	}
	return nil
}
