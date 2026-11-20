package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// DownloadAccessTransaction 提供同一用户事务内的可见路径、内容引用和访问写入。
type DownloadAccessTransaction interface {
	RecordAccessTransaction
	// FindUserFileEntryContent 读取文件当前绑定的对象引用。
	FindUserFileEntryContent(ctx context.Context, userID, entryID uint64) (*domain.FileEntryContent, error)
}

// recordDownloadAccess 在已认证用户事务中复查签名绑定并合并记录最近访问。
func (downloadService *EntryDownloadService) recordDownloadAccess(ctx context.Context, actor domain.Actor, target *domain.EntryDownloadTarget) error {
	// 1. 在用户事务中重新检查可见文件及签名绑定的版本。
	recorded := false
	transactionError := downloadService.withAccessTransaction(ctx, actor, func(txContext context.Context, txStore DownloadAccessTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("记录下载访问需要事务上下文和读取能力")
		}
		path, err := readFileEntryPath(txContext, txStore, actor.UserID, target.EntryID)
		if err != nil {
			return err
		}
		entry := path.Entries[len(path.Entries)-1]
		if entry.EntryType != "FILE" {
			return domain.ErrFileEntryNotFile
		}
		if err := ValidateEntryRevision(entry.Revision, target.Revision); err != nil {
			return err
		}

		// 2. 确认对象引用一致，按 60 秒窗口合并访问时间。
		content, err := txStore.FindUserFileEntryContent(txContext, actor.UserID, target.EntryID)
		if err := txContext.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		if content == nil || content.ID != target.EntryID || content.UserID != actor.UserID {
			return domain.ErrInvalidFileEntryData
		}
		if content.ObjectID == nil || *content.ObjectID != target.ObjectID {
			return domain.ErrDownloadTargetUnavailable
		}
		preference, err := txStore.RecordUserEntryAccess(txContext, target.EntryID)
		if contextError := txContext.Err(); contextError != nil {
			return contextError
		}
		if err != nil {
			return err
		}
		if preference == nil || preference.UserID != actor.UserID || preference.EntryID != target.EntryID ||
			preference.LastOpenedAt == nil || preference.LastOpenedAt.IsZero() {
			return domain.ErrInvalidFileEntryData
		}
		recorded = true
		return nil
	})

	// 3. 确认整个用户事务成功提交。
	if transactionError != nil {
		return fmt.Errorf("记录下载访问失败: %w", transactionError)
	}
	if !recorded {
		return domain.ErrInvalidFileEntryData
	}
	return nil
}
