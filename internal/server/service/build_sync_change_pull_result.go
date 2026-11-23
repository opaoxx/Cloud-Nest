package service

import (
	"bytes"
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// BuildSyncChangePullResult 从已校验的页面构造续读结果。
// 调用方须在读取事务成功结束后使用外层请求上下文，并保持原始页面不变。
func BuildSyncChangePullResult(ctx context.Context, page *SyncChangeReadPage, signer *SyncChangeCursorSigner) (*domain.SyncChangePullResult, error) {
	// 1. 检查已校验页面的边界和请求状态。
	if ctx == nil {
		return nil, fmt.Errorf("交付增量页需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if page == nil {
		return nil, domain.ErrInvalidSyncChangePage
	}
	nextPosition := page.startPosition
	if err := ValidateSyncChangePageSequence(nextPosition.Sequence, page.HighWaterSequence, page.pageSize, page.Events); err != nil {
		return nil, err
	}

	// 2. 非空页取最后一条序号，空页沿用原位置，再签发游标。
	if count := len(page.Events); count > 0 {
		nextPosition.Sequence = page.Events[count-1].Sequence
	}
	nextCursor, err := signer.Sign(nextPosition)
	if err != nil {
		return nil, fmt.Errorf("签发增量页续读游标失败: %w", err)
	}

	// 3. 生成带游标和后续标记的完整结果。
	events := make([]domain.SyncChangeEvent, len(page.Events))
	for index, event := range page.Events {
		event.Payload = bytes.Clone(event.Payload)
		events[index] = event
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &domain.SyncChangePullResult{
		Events:            events,
		HighWaterSequence: page.HighWaterSequence,
		NextCursor:        nextCursor,
		HasMore:           nextPosition.Sequence < page.HighWaterSequence,
	}, nil
}
