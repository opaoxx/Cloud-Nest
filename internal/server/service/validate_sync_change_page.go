package service

import (
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateSyncChangePage 校验增量页的完整性和全部事件载荷。
func ValidateSyncChangePage(position, highWaterSequence uint64, pageSize int, events []domain.SyncChangeEvent) error {
	// 1. 复用页内条数和序号连续性校验。
	if err := ValidateSyncChangePageSequence(position, highWaterSequence, pageSize, events); err != nil {
		return err
	}

	// 2. 沿原顺序按事件类型校验四字段或六字段载荷。
	for _, event := range events {
		var err error
		switch event.EventType {
		case domain.FilePublishedEventType, domain.FileContentReplacedEventType:
			err = domain.ValidateSyncChangeContentEvent(event)
		default:
			err = domain.ValidateSyncChangeMetadataEvent(event)
		}

		// 3. 任一事件异常时返回带序号的错误。
		if err != nil {
			return fmt.Errorf("校验序号 %d 的增量事件失败: %w", event.Sequence, err)
		}
	}
	return nil
}
