package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateCreateSyncSnapshotReplay 复核已查到的快照创建回执并交付原快照定位数据。
// 调用方先复查授权并使用认证设备和本次页大小计算 requestHash。
func ValidateCreateSyncSnapshotReplay(
	record *domain.IdempotencyRecord, actor domain.Actor, key string, requestHash [sha256.Size]byte, now time.Time,
	decodeReceipt func(json.RawMessage) (*domain.SyncSnapshotReceiptLocator, error),
) (*domain.SyncSnapshotReceiptLocator, error) {
	// 1. 核对调用参数，沿用记录范围、期限与请求摘要的检查顺序。
	if actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 ||
		!domain.ValidIdempotencyKey(key) || now.IsZero() || decodeReceipt == nil {
		return nil, fmt.Errorf("复核快照创建回执需要完整认证身份、有效幂等键、判断时间及回执解码方法")
	}
	if err := validateEntryReplayRecord(record, actor.UserID, domain.CreateSyncSnapshotOperation, key, requestHash, now); err != nil {
		return nil, err
	}

	// 2. 确认保存的是快照创建成功回执，UUID 由 JSON 正文保存。
	if record.ResourceType != domain.SyncSnapshotResourceType || record.ResourceID != nil ||
		record.ResponseStatus != http.StatusCreated || len(record.ResponseBody) == 0 {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 3. 解码定位数据，核对正文中的设备与本次认证设备一致。
	locator, err := decodeReceipt(record.ResponseBody)
	if err != nil || locator == nil || locator.DeviceID != actor.DeviceID {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return locator, nil
}
