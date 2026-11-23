package service

import (
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateSyncChangePageSequence 核对从已复核起点读取的一页事件是否完整且连续。
func ValidateSyncChangePageSequence(position, highWaterSequence uint64, pageSize int, events []domain.SyncChangeEvent) error {
	// 1. 校验起点、上界和页大小，再计算本页应读条数。
	if position > highWaterSequence || pageSize < 1 || pageSize > 100 {
		return fmt.Errorf("%w: 起点 %d、上界 %d 或页大小 %d 无效", domain.ErrInvalidSyncChangePage, position, highWaterSequence, pageSize)
	}
	expectedCount := min(uint64(pageSize), highWaterSequence-position)

	// 2. 核对实际条数，拒绝缺行或多行。
	if uint64(len(events)) != expectedCount {
		return fmt.Errorf("%w: 应读 %d 条，实际读取 %d 条", domain.ErrInvalidSyncChangePage, expectedCount, len(events))
	}

	// 3. 从起点的下一个序号逐条比对，整页连续才通过。
	for index, event := range events {
		expectedSequence := position + uint64(index) + 1
		if event.Sequence != expectedSequence {
			return fmt.Errorf("%w: 预期序号 %d，实际为 %d", domain.ErrInvalidSyncChangePage, expectedSequence, event.Sequence)
		}
	}
	return nil
}
