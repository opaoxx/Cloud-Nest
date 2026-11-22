package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncSnapshotFinalizationTransaction 提供最终账号事务内的回执查询、复核和写入。
type SyncSnapshotFinalizationTransaction interface {
	SyncSnapshotReceiptReader
	SyncSnapshotCreationTransaction
}

// FinalizeSyncSnapshotCreationInTransaction 在账号锁内确定复用原快照或保存本次准备的快照。
// 返回非空定位表示已有回执。返回 nil, nil 表示已写入本次准备结果，仍须等待外层事务提交。
// now 由调用方在取得账号锁后读取。
func FinalizeSyncSnapshotCreationInTransaction(
	ctx context.Context, actor domain.Actor, key string, prepared *PreparedSyncSnapshot, now time.Time,
	transaction SyncSnapshotFinalizationTransaction,
	encodeReceipt func(domain.SyncSnapshotReceiptLocator) (json.RawMessage, error),
	decodeReceipt func(json.RawMessage) (*domain.SyncSnapshotReceiptLocator, error),
) (*domain.SyncSnapshotReceiptLocator, error) {
	// 1. 校验准备结果，计算本次请求摘要。
	if prepared == nil || transaction == nil || encodeReceipt == nil || prepared.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("完成同步快照创建需要准备结果、账号事务及回执编码方法")
	}
	binding := prepared.Binding
	if !binding.valid() || binding.UserID != actor.UserID || binding.DeviceID != actor.DeviceID || prepared.Header.RootEntryID == 0 {
		return nil, domain.ErrInvalidSyncDeviceData
	}
	requestHash, err := BuildCreateSyncSnapshotRequestHash(binding.DeviceID, binding.PageSize)
	if err != nil {
		return nil, err
	}

	// 2. 在账号锁内复查同键回执，命中则返回原定位。
	existing, err := FindCreateSyncSnapshotReplayInTransaction(
		ctx, actor, key, requestHash, now, transaction.FindUserIdempotencyRecord, decodeReceipt,
	)
	if err != nil || existing != nil {
		return existing, err
	}

	// 3. 确认候选仍有效，再保存设备代次和创建回执。
	if !now.Before(prepared.ExpiresAt) {
		return nil, domain.ErrSyncSnapshotExpired
	}
	return nil, PersistSyncSnapshotCreationInTransaction(
		ctx, actor, binding, prepared.Header.HighWaterSequence, key, now, transaction, encodeReceipt, decodeReceipt,
	)
}
