package service

import (
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateTrashEntryReplay 检查保存的记录是否对应当前单项回收请求。
// 调用方先完成认证、生成请求摘要并按用户范围读取记录，通过后恢复完整快照。
func ValidateTrashEntryReplay(record *domain.IdempotencyRecord, userID, entryID, expectedRevision uint64, key string, requestHash [sha256.Size]byte, now time.Time) error {
	// 1. 校验调用参数及记录归属。
	if userID == 0 || entryID == 0 || expectedRevision == 0 || !domain.ValidIdempotencyKey(key) || now.IsZero() {
		return fmt.Errorf("校验回收幂等结果需要非零用户、目标和预期版本、有效幂等键及判断时间")
	}
	return validateEntryRevisionReplay(record, userID, entryID, expectedRevision, domain.TrashEntryOperation, key, requestHash, now)
}
