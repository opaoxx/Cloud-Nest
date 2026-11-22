package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ApplyUploadReplacementContent 在上传写回复核事务中保留旧内容、切换引用并结算新内容预占。
// 调用方须在同一事务保护发布对象、完成任务并记录事件。任一步失败均交给外层回滚。
func (store *Store) ApplyUploadReplacementContent(
	ctx context.Context, task *domain.UploadTask, objectID uint64, digest domain.UploadContentDigest, retentionDays int,
) (*domain.FileEntry, error) {
	// 1. 确认账号和任务仍符合写回复核条件，使用锁内的替换目标和版本。
	if store == nil || store.database == nil || store.loginUserID == 0 || ctx == nil {
		return nil, fmt.Errorf("写入替换内容需要账号事务和上下文")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("写入替换内容必须使用账号事务 Store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if task == nil || task.ReplacementEntryID == nil || task.ReplacementExpectedRevision == nil ||
		task.ExpectedSHA256 == nil || *task.ExpectedSHA256 != digest.SHA256 || task.FileSize != digest.SizeBytes {
		return nil, domain.ErrInvalidUploadTaskData
	}
	lockedStore := *store
	lockedStore.database = store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"})
	current, err := lockedStore.findVerifyingUploadWithFullReservation(ctx, task, task.UploadMode)
	if err != nil {
		return nil, err
	}
	if current.ReplacementEntryID == nil || *current.ReplacementEntryID != *task.ReplacementEntryID ||
		current.ReplacementExpectedRevision == nil || *current.ReplacementExpectedRevision != *task.ReplacementExpectedRevision ||
		current.ExpectedSHA256 == nil || *current.ExpectedSHA256 != digest.SHA256 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	entryID, expectedRevision := *current.ReplacementEntryID, *current.ReplacementExpectedRevision

	// 2. 在同一事务保存等待被替换的内容，再按原版本切换当前引用。
	if _, err := store.SaveCurrentFileVersion(ctx, entryID, expectedRevision, retentionDays); err != nil {
		return nil, err
	}
	updated, err := store.ReplaceUploadFileContent(ctx, entryID, expectedRevision, objectID, digest)
	if err != nil {
		return nil, err
	}

	// 3. 只把本次任务的全额预占转为已用，历史继承原内容的用量。
	if err := store.SettlePublishedUploadQuota(ctx, current); err != nil {
		return nil, err
	}
	return updated, nil
}
