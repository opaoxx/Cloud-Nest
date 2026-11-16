package service

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SetStarredTransaction 汇集收藏设置使用的事务内路径读取与偏好写入能力。
type SetStarredTransaction interface {
	FileEntryPathReader
	// SetUserEntryStarred 设置当前用户偏好，结果由外层事务确认提交。
	SetUserEntryStarred(ctx context.Context, entryID uint64, starred bool) (*domain.EntryPreference, error)
}

// SetStarredService 在同一已认证用户事务内检查完整路径并设置收藏。
type SetStarredService struct {
	// withTransaction 同步执行回调，传播失败，仅在确认提交后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, SetStarredTransaction) error) error
}

// NewSetStarredService 绑定已有已认证用户事务入口。
func NewSetStarredService(withTransaction func(context.Context, domain.Actor, func(context.Context, SetStarredTransaction) error) error) *SetStarredService {
	// 1. 检查并保存收藏设置所需的事务方法。
	if withTransaction == nil {
		panic("收藏服务需要已认证用户事务方法")
	}
	return &SetStarredService{withTransaction: withTransaction}
}

// Set 设置可见文件、目录或根目录的收藏状态，确认提交后交付独立偏好。
func (starredService *SetStarredService) Set(ctx context.Context, actor domain.Actor, entryID uint64, starred bool) (*domain.EntryPreference, error) {
	// 1. 检查上下文、完整身份、目标 ID 和取消状态。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || entryID == 0 {
		return nil, fmt.Errorf("设置收藏需要上下文、完整认证身份和非零目标 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 进入用户事务，检查目标及全部祖先的可见性。
	var prepared *domain.EntryPreference
	transactionError := starredService.withTransaction(ctx, actor, func(txContext context.Context, txStore SetStarredTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("收藏回调需要事务上下文和 Store")
		}
		paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
			return action(ctx, txStore)
		})
		if _, err := paths.Get(txContext, actor, entryID); err != nil {
			return err
		}

		// 3. 设置收藏，核对归属、目标、状态及时间。
		preference, writeError := txStore.SetUserEntryStarred(txContext, entryID, starred)
		if err := txContext.Err(); err != nil {
			return err
		}
		if writeError != nil {
			return fmt.Errorf("设置目录项收藏失败: %w", writeError)
		}
		if preference == nil || preference.UserID != actor.UserID || preference.EntryID != entryID || (preference.StarredAt != nil) != starred {
			return domain.ErrInvalidFileEntryData
		}
		for _, stamp := range []*time.Time{preference.StarredAt, preference.LastOpenedAt} {
			if stamp != nil && (stamp.IsZero() || stamp.UTC().Year() < 1000 || stamp.UTC().Year() > 9999) {
				return domain.ErrInvalidFileEntryData
			}
		}

		// 4. 复制可空时间，准备独立结果。
		preferenceCopy := *preference
		preferenceCopy.StarredAt = copyOptionalValue(preference.StarredAt)
		preferenceCopy.LastOpenedAt = copyOptionalValue(preference.LastOpenedAt)
		prepared = &preferenceCopy
		return nil
	})

	// 5. 确认外层事务提交成功，返回偏好。
	if transactionError != nil {
		return nil, fmt.Errorf("收藏事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
