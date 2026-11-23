package service

import (
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncChangePullStart 保存已验签的排他读取起点，供一致性视图复核。
type SyncChangePullStart struct {
	Position domain.SyncChangeCursorPosition
	// BaselineExpiresAt 保留首次拉取的原期限。增量游标续读时为零值。
	BaselineExpiresAt time.Time
}

// SyncChangePullStartResolver 将首次和后续拉取凭据解析为同一种起点。
type SyncChangePullStartResolver struct {
	baselineVerifier *SyncSnapshotBaselineVerifier
	cursorVerifier   *SyncChangeCursorVerifier
	now              func() time.Time
}

// NewSyncChangePullStartResolver 绑定两种用途的验签器和首次拉取的时钟。
func NewSyncChangePullStartResolver(baselineVerifier *SyncSnapshotBaselineVerifier, cursorVerifier *SyncChangeCursorVerifier, now func() time.Time) *SyncChangePullStartResolver {
	if baselineVerifier == nil || cursorVerifier == nil || now == nil {
		panic("解析增量拉取起点需要末页凭据验签器、增量游标验签器和时钟")
	}
	return &SyncChangePullStartResolver{baselineVerifier: baselineVerifier, cursorVerifier: cursorVerifier, now: now}
}

// Resolve 验签后返回起点。数据库中的当前代次和可续读边界由调用方复核。
func (resolver *SyncChangePullStartResolver) Resolve(actor domain.Actor, baselineToken, cursor string) (*SyncChangePullStart, error) {
	// 1. 校验认证身份，确保只提供一种凭据。
	if actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	if (baselineToken == "") == (cursor == "") {
		return nil, fmt.Errorf("解析增量拉取起点需要且仅需要一种凭据")
	}

	// 2. 后续拉取使用独立的增量游标，按已交付位置续读。
	if cursor != "" {
		position, err := resolver.cursorVerifier.Verify(actor.UserID, actor.DeviceID, cursor)
		if err != nil {
			return nil, err
		}
		return &SyncChangePullStart{Position: *position}, nil
	}

	// 3. 首次拉取使用快照截止位置，并保留原期限供读取时复核。
	baseline, err := resolver.baselineVerifier.VerifyForPull(actor.UserID, actor.DeviceID, baselineToken, resolver.now())
	if err != nil {
		return nil, err
	}
	return &SyncChangePullStart{
		Position: domain.SyncChangeCursorPosition{
			UserID: baseline.UserID, DeviceID: baseline.DeviceID, Generation: baseline.Generation, Sequence: baseline.HighWaterSequence,
		},
		BaselineExpiresAt: baseline.ExpiresAt,
	}, nil
}
