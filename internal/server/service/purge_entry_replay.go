package service

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidatePurgeEntryReplay 检查保存的 202 记录是否可用于本次确认。
// decodeEntryID 校验完整快照并返回其中的十进制目标 ID。
func ValidatePurgeEntryReplay(
	record *domain.IdempotencyRecord,
	userID, entryID uint64,
	key string,
	requestHash [sha256.Size]byte,
	now time.Time,
	decodeEntryID func([]byte) (string, error),
) error {
	// 1. 检查调用参数。
	if userID == 0 || entryID == 0 || !domain.ValidIdempotencyKey(key) || now.IsZero() || decodeEntryID == nil {
		return fmt.Errorf("校验永久清理幂等结果需要非零用户和目标、有效幂等键、判断时间及快照解码方法")
	}

	// 2. 核对记录范围，检查记录期限，再比较请求摘要。
	if err := validateEntryReplayRecord(record, userID, domain.PurgeEntryOperation, key, requestHash, now); err != nil {
		return err
	}

	// 3. 核对 202 记录，解码快照并比较目标 ID。
	if record.ResourceType != "ENTRY" || record.ResourceID == nil || *record.ResourceID != entryID ||
		record.ResponseStatus != 202 || len(record.ResponseBody) == 0 {
		return domain.ErrInvalidIdempotencyRecordData
	}
	savedEntryID, err := decodeEntryID(record.ResponseBody)
	if err != nil || savedEntryID != strconv.FormatUint(entryID, 10) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	return nil
}
