package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// FindCreateSyncSnapshotReplayInTransaction 查找并复核已认证账号的快照创建回执。
// findRecord 须绑定当前账号锁内的事务 Store。仅在明确没有记录时返回 nil, nil。
func FindCreateSyncSnapshotReplayInTransaction(
	ctx context.Context, actor domain.Actor, key string, requestHash [sha256.Size]byte, now time.Time,
	findRecord func(context.Context, uint64, string, string) (*domain.IdempotencyRecord, error),
	decodeReceipt func(json.RawMessage) (*domain.SyncSnapshotReceiptLocator, error),
) (*domain.SyncSnapshotReceiptLocator, error) {
	// 1. 校验请求和事务依赖，按账号、操作名及键查回执。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 ||
		!domain.ValidIdempotencyKey(key) || now.IsZero() || findRecord == nil || decodeReceipt == nil {
		return nil, fmt.Errorf("查找快照创建回执需要上下文、完整认证身份、有效幂等键、判断时间及事务查询和解码方法")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	record, queryError := findRecord(ctx, actor.UserID, domain.CreateSyncSnapshotOperation, key)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 区分查无记录和查询失败。矛盾的返回值按记录异常处理。
	if errors.Is(queryError, domain.ErrIdempotencyRecordNotFound) {
		if record != nil {
			return nil, domain.ErrInvalidIdempotencyRecordData
		}
		return nil, nil
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询快照创建回执失败: %w", queryError)
	}

	// 3. 复核已有回执，交付原快照定位数据。
	locator, err := ValidateCreateSyncSnapshotReplay(record, actor, key, requestHash, now, decodeReceipt)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	return locator, err
}
