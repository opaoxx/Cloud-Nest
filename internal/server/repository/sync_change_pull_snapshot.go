package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// WithSyncChangePullSnapshot 在只读一致性事务内复核身份并读取设备的同步状态。
// actor 须来自认证。回调仅在执行期间使用传入的上下文和 Store。
func (store *Store) WithSyncChangePullSnapshot(ctx context.Context, actor domain.Actor, action func(context.Context, *Store, domain.SyncDeviceState) error) error {
	// 1. 校验认证身份和回调，复用五秒只读可重复读事务。
	if ctx == nil || action == nil {
		return fmt.Errorf("增量拉取读视图需要上下文和回调")
	}
	if actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return domain.ErrInvalidSessionData
	}
	return store.WithFileEntrySnapshot(ctx, func(readContext context.Context, reader *Store) error {
		// 2. 先在该视图核对当前授权及设备归属，再读取同步状态。
		authorization, err := reader.FindSessionAuthorization(readContext, actor.UserID, actor.SessionID)
		if err != nil {
			return err
		}
		if authorization.DeviceID != actor.DeviceID {
			return domain.ErrSessionNotFound
		}
		switch authorization.UserStatus {
		case "ACTIVE":
		case "DISABLED":
			return domain.ErrAccountDisabled
		default:
			return domain.ErrInvalidAccountData
		}
		if authorization.SessionRevokedAt != nil {
			return domain.ErrSessionRevoked
		}
		if authorization.DeviceRevokedAt != nil {
			return domain.ErrDeviceRevoked
		}
		if authorization.SessionExpiresAt.IsZero() {
			return domain.ErrInvalidSessionData
		}
		if !authorization.SessionExpiresAt.After(time.Now()) {
			return domain.ErrSessionExpired
		}
		state, err := reader.readSyncDeviceState(readContext, actor, false)
		if err != nil {
			return err
		}

		// 3. 把已核对的设备状态交给调用方，后续读取继续使用同一事务。
		reader.syncChangePullUserID = actor.UserID
		return action(readContext, reader, state)
	})
}
