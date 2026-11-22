package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncSnapshotPageDelivery 为已读取的快照页签发本页应交付的凭据。
type SyncSnapshotPageDelivery struct {
	signCursor   func(SyncSnapshotCacheBinding, int, time.Time) (string, error)
	signBaseline func(SyncSnapshotCacheBinding, uint64, time.Time) (string, error)
}

// NewSyncSnapshotPageDelivery 装配续页与末页确认签发器。
func NewSyncSnapshotPageDelivery(
	signCursor func(SyncSnapshotCacheBinding, int, time.Time) (string, error),
	signBaseline func(SyncSnapshotCacheBinding, uint64, time.Time) (string, error),
) *SyncSnapshotPageDelivery {
	if signCursor == nil || signBaseline == nil {
		panic("交付同步快照页需要续页与末页确认两种签发器")
	}
	return &SyncSnapshotPageDelivery{signCursor: signCursor, signBaseline: signBaseline}
}

// Build 从已核验的快照页准备完整交付结果，适用于首页及续页。
func (delivery *SyncSnapshotPageDelivery) Build(ctx context.Context, binding SyncSnapshotCacheBinding, header domain.SyncSnapshotHeader, expiresAt time.Time, page *SyncSnapshotPage) (*domain.SyncSnapshotPageResult, error) {
	// 1. 校验快照元数据和页边界，复制本页条目。
	if ctx == nil {
		return nil, fmt.Errorf("交付同步快照页需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	expiresAt = expiresAt.UTC()
	if !binding.valid() || header.RootEntryID == 0 || expiresAt.Year() < 1000 || expiresAt.Year() > 9999 ||
		page == nil || len(page.Items) > binding.PageSize || (page.NextPosition != nil && len(page.Items) != binding.PageSize) {
		return nil, domain.ErrInvalidFileEntryData
	}
	result := &domain.SyncSnapshotPageResult{
		SnapshotID: binding.SnapshotID, Header: header, ExpiresAt: expiresAt,
		Items: make([]json.RawMessage, len(page.Items)),
	}
	for index, item := range page.Items {
		if len(item) < 2 || item[0] != '{' || !json.Valid(item) {
			return nil, domain.ErrInvalidFileEntryData
		}
		result.Items[index] = bytes.Clone(item)
	}

	// 2. 有下一位置就签发续页凭据，否则签发末页确认凭据。
	var token string
	var signError error
	if page.NextPosition != nil {
		token, signError = delivery.signCursor(binding, *page.NextPosition, expiresAt)
		result.NextCursor = &token
	} else {
		token, signError = delivery.signBaseline(binding, header.HighWaterSequence, expiresAt)
		result.BaselineToken = &token
	}

	// 3. 签发和上下文均成功后才返回完整结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if signError != nil {
		return nil, fmt.Errorf("签发同步快照页凭据失败: %w", signError)
	}
	if token == "" || len(token) > domain.MaxSyncSnapshotTokenLength {
		return nil, fmt.Errorf("同步快照页签发结果无效")
	}
	return result, nil
}
