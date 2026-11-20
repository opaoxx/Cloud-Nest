package service

import (
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateRestoreEntryReplay 检查已保存的结果是否对应本次单项恢复。
// 调用方先在认证用户范围读取记录，校验成功后再解码完整快照。
func ValidateRestoreEntryReplay(record *domain.IdempotencyRecord, userID, entryID, expectedRevision uint64, key string, requestHash [sha256.Size]byte, now time.Time) error {
	// 1. 核对用户、恢复操作和幂等键。
	if userID == 0 || entryID == 0 || expectedRevision == 0 || !domain.ValidIdempotencyKey(key) || now.IsZero() {
		return fmt.Errorf("校验恢复幂等结果需要非零用户、目标和预期版本、有效幂等键及判断时间")
	}

	return validateEntryRevisionReplay(record, userID, entryID, expectedRevision, domain.RestoreEntryOperation, key, requestHash, now)
}
