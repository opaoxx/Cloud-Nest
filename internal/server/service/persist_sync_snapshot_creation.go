package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const syncSnapshotCreationReceiptRetention = 8 * 24 * time.Hour

// SyncSnapshotCreationTransaction 提供已认证账号事务内的截止序号复核和两项写入。
type SyncSnapshotCreationTransaction interface {
	ValidateSyncSnapshotSequence(context.Context, uint64) error
	ResetSyncDeviceBaseline(context.Context, domain.SyncCursorGeneration) error
	CreateUserIdempotencyRecord(context.Context, domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error)
}

// PersistSyncSnapshotCreationInTransaction 在同一个账号事务内重置当前设备并保存创建回执。
// 调用方须在账号锁内确认幂等键尚无记录，将失败交给外层事务，并在提交后才交付快照。
func PersistSyncSnapshotCreationInTransaction(
	ctx context.Context, actor domain.Actor, binding SyncSnapshotCacheBinding, highWaterSequence uint64,
	key string, now time.Time, transaction SyncSnapshotCreationTransaction,
	encodeReceipt func(domain.SyncSnapshotReceiptLocator) (json.RawMessage, error),
	decodeReceipt func(json.RawMessage) (*domain.SyncSnapshotReceiptLocator, error),
) error {
	// 1. 核对快照绑定，准备请求摘要和回执正文。
	if ctx == nil || transaction == nil || encodeReceipt == nil || decodeReceipt == nil ||
		now.IsZero() || !domain.ValidIdempotencyKey(key) {
		return fmt.Errorf("保存同步快照创建结果需要事务、有效幂等键、当前时间和回执编解码")
	}
	if actor.SessionID == 0 || binding.UserID != actor.UserID || binding.DeviceID != actor.DeviceID || !binding.valid() {
		return domain.ErrInvalidSyncDeviceData
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	requestHash, err := BuildCreateSyncSnapshotRequestHash(binding.DeviceID, binding.PageSize)
	if err != nil {
		return err
	}
	locator := domain.SyncSnapshotReceiptLocator{
		SnapshotID: binding.SnapshotID, DeviceID: binding.DeviceID, Generation: binding.Generation,
	}
	body, err := encodeReceipt(locator)
	if err != nil {
		return fmt.Errorf("编码同步快照创建回执失败: %w", err)
	}

	// 2. 复核截止序号，再重置当前设备的基线。
	if err := transaction.ValidateSyncSnapshotSequence(ctx, highWaterSequence); err != nil {
		return err
	}
	if err := transaction.ResetSyncDeviceBaseline(ctx, binding.Generation); err != nil {
		return err
	}

	// 3. 保存并复核创建回执，将结果交给外层事务。
	expiresAt := now.UTC().Truncate(time.Microsecond).Add(syncSnapshotCreationReceiptRetention)
	record, err := transaction.CreateUserIdempotencyRecord(ctx, domain.IdempotencyResultInput{
		Operation: domain.CreateSyncSnapshotOperation, IdempotencyKey: key, RequestHash: requestHash,
		ResourceType: domain.SyncSnapshotResourceType, ResponseStatus: http.StatusCreated,
		ResponseBody: body, ExpiresAt: expiresAt,
	})
	if err != nil {
		return fmt.Errorf("保存同步快照创建回执失败: %w", err)
	}
	savedLocator, err := ValidateCreateSyncSnapshotReplay(record, actor, key, requestHash, now, decodeReceipt)
	if err != nil {
		return err
	}
	if *savedLocator != locator || !record.ExpiresAt.Equal(expiresAt) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	return ctx.Err()
}
