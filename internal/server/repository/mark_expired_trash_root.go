package repository

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// MarkExpiredTrashRootPurging 复核单条到期候选并提交清理开始状态和变更事件。
// 成功后旧扫描候选失效，返回的清理根项已提交。
func (store *Store) MarkExpiredTrashRootPurging(ctx context.Context, candidate domain.ExpiredTrashRootCandidate) (*domain.PurgeRootCandidate, error) {
	// 1. 锁定并复核单条到期候选。
	var marked *domain.PurgeRootCandidate
	err := store.WithExpiredTrashRootTransaction(ctx, candidate, func(txContext context.Context, txStore *Store, current domain.ExpiredTrashRootCandidate) error {
		if current.Revision == math.MaxUint64 {
			return domain.ErrEntryRevisionExhausted
		}

		// 2. 匹配原状态、版本和回收时间，条件写入 PURGING。
		result := txStore.database.WithContext(txContext).Exec(`
			UPDATE file_entries
			SET status = 'PURGING', purge_started_at = UTC_TIMESTAMP(6), revision = revision + 1,
			    modified_at = UTC_TIMESTAMP(6), updated_at = UTC_TIMESTAMP(6)
			WHERE user_id = ? AND id = ? AND parent_id = ? AND status = 'TRASHED' AND revision = ?
			  AND deleted_at = ? AND deleted_at < DATE_SUB(UTC_TIMESTAMP(6), INTERVAL ? DAY)`,
			current.UserID, current.EntryID, current.ParentID, current.Revision, current.DeletedAt, domain.TrashRetentionDays)
		if err := txContext.Err(); err != nil {
			return err
		}
		if result.Error != nil {
			return fmt.Errorf("标记到期回收站根项失败: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return domain.ErrExpiredTrashRootCandidateStale
		}
		if result.RowsAffected != 1 {
			return domain.ErrInvalidFileEntryData
		}

		// 3. 读回根项，核对新版本和清理开始时间。
		saved, err := txStore.readLockedTrashEntry(txContext, current.UserID, current.EntryID)
		if errors.Is(err, domain.ErrFileEntryNotFound) {
			return domain.ErrInvalidFileEntryData
		}
		if err != nil {
			return err
		}
		if saved.Status != "PURGING" || saved.Revision != current.Revision+1 ||
			saved.ParentID == nil || *saved.ParentID != current.ParentID ||
			saved.DeletedAt == nil || !saved.DeletedAt.Equal(current.DeletedAt) || saved.PurgeStartedAt == nil {
			return domain.ErrInvalidFileEntryData
		}
		root := domain.PurgeRootCandidate{
			EntryID: saved.ID, UserID: saved.UserID, ParentID: *saved.ParentID,
			Revision: saved.Revision, PurgeStartedAt: saved.PurgeStartedAt.UTC(),
		}
		if !root.IsValid() || !saved.ModifiedAt.Equal(root.PurgeStartedAt) || !saved.UpdatedAt.Equal(root.PurgeStartedAt) ||
			!root.PurgeStartedAt.After(current.DeletedAt.AddDate(0, 0, domain.TrashRetentionDays)) {
			return domain.ErrInvalidFileEntryData
		}

		// 4. 使用实际写回结果追加事件，失败则和本次标记一起回滚。
		event, err := domain.NewEntryChangeEvent(domain.EntryPurgeStartedEventType, &domain.FileEntry{
			ID: saved.ID, ParentID: saved.ParentID, Name: saved.Name, Revision: saved.Revision,
		})
		if err != nil {
			return fmt.Errorf("编码到期清理开始事件失败: %w", err)
		}
		if _, err := txStore.AppendUserChangeEvent(txContext, event); err != nil {
			return fmt.Errorf("追加到期清理开始事件失败: %w", err)
		}
		marked = &root
		return nil
	})
	// 5. 仅在事务提交成功后返回清理根项。
	if err != nil {
		return nil, err
	}
	return marked, nil
}
