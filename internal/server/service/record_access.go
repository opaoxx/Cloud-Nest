package service

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RecordAccessTransaction 汇集访问记录使用的事务内路径读取和偏好写入能力。
type RecordAccessTransaction interface {
	FileEntryPathReader
	// RecordUserEntryAccess 合并记录当前用户文件访问，结果由外层确认提交。
	RecordUserEntryAccess(ctx context.Context, entryID uint64) (*domain.EntryPreference, error)
}

// RecordAccessService 在同一已认证用户事务中检查用途、路径并记录文件访问。
type RecordAccessService struct {
	// withTransaction 同步执行回调，传播失败，仅在确认提交后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, RecordAccessTransaction) error) error
}

// NewRecordAccessService 绑定已有已认证用户事务入口。
func NewRecordAccessService(withTransaction func(context.Context, domain.Actor, func(context.Context, RecordAccessTransaction) error) error) *RecordAccessService {
	// 1. 检查并保存访问记录所需的事务方法。
	if withTransaction == nil {
		panic("访问记录服务需要已认证用户事务方法")
	}
	return &RecordAccessService{withTransaction: withTransaction}
}

// Record 为可见文件记录 preview/open 访问，确认提交后交付独立偏好。
func (accessService *RecordAccessService) Record(ctx context.Context, actor domain.Actor, entryID uint64, purpose string) (*domain.EntryPreference, error) {
	// 1. 检查上下文、完整身份、目标 ID 和访问用途。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || entryID == 0 {
		return nil, fmt.Errorf("记录访问需要上下文、完整认证身份和非零目标 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidEntryAccessPurpose(purpose) {
		return nil, domain.ErrInvalidEntryAccessPurpose
	}

	// 2. 进入用户事务，检查目标及全部祖先的可见性。
	var prepared *domain.EntryPreference
	transactionError := accessService.withTransaction(ctx, actor, func(txContext context.Context, txStore RecordAccessTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("访问记录回调需要事务上下文和 Store")
		}
		paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
			return action(ctx, txStore)
		})
		path, err := paths.Get(txContext, actor, entryID)
		if err != nil {
			return err
		}

		// 3. 检查目标为文件。
		if path.Entries[len(path.Entries)-1].EntryType != "FILE" {
			return domain.ErrFileEntryNotFile
		}

		// 4. 记录访问，核对归属、目标和时间。
		preference, writeError := txStore.RecordUserEntryAccess(txContext, entryID)
		if err := txContext.Err(); err != nil {
			return err
		}
		if writeError != nil {
			return fmt.Errorf("记录文件最近访问失败: %w", writeError)
		}
		if preference == nil || preference.UserID != actor.UserID || preference.EntryID != entryID || preference.LastOpenedAt == nil {
			return domain.ErrInvalidFileEntryData
		}
		for _, stamp := range []*time.Time{preference.StarredAt, preference.LastOpenedAt} {
			if stamp != nil && (stamp.IsZero() || stamp.UTC().Year() < 1000 || stamp.UTC().Year() > 9999) {
				return domain.ErrInvalidFileEntryData
			}
		}

		// 5. 复制可空时间，准备独立结果。
		preferenceCopy := *preference
		preferenceCopy.StarredAt = copyOptionalValue(preference.StarredAt)
		preferenceCopy.LastOpenedAt = copyOptionalValue(preference.LastOpenedAt)
		prepared = &preferenceCopy
		return nil
	})

	// 6. 确认外层事务提交成功，返回偏好。
	if transactionError != nil {
		return nil, fmt.Errorf("访问记录事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
