package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm/clause"
)

// RecheckMultipartUploadSessionCleanup 在账号锁内复核原会话的数据库条件。
// 当前仍在 CREATING 时一律推迟，直到初始化入口无法再绑定该任务。
func (store *Store) RecheckMultipartUploadSessionCleanup(
	ctx context.Context, provider, bucket string, target domain.MultipartUploadSessionScanTarget,
	session domain.MultipartUploadSession, gracePeriod time.Duration,
) (domain.MultipartUploadSessionLockedCheck, error) {
	// 1. 校验候选范围，进入所属账号事务。禁用账号仍允许内部复核其遗留会话。
	var check domain.MultipartUploadSessionLockedCheck
	if store == nil || store.database == nil || ctx == nil {
		return check, fmt.Errorf("加锁复核分片会话需要数据入口和上下文")
	}
	if err := validateMultipartUploadSessionBindingScope(provider, bucket, target, session); err != nil {
		return check, err
	}
	if err := validateMultipartUploadSessionGrace(session, gracePeriod); err != nil {
		return check, err
	}

	const allowDisabled = true
	err := store.withLockedUserTransaction(ctx, target.UserID, allowDisabled, func(txContext context.Context, txStore *Store) error {
		// 2. 锁定同一任务及原对象，从同一条查询读取数据库时间。
		lockedQuery := txStore.database.Clauses(clause.Locking{Strength: "UPDATE"})
		row, err := findMultipartUploadSessionBinding(txContext, lockedQuery, provider, bucket, target)
		if err != nil {
			return err
		}
		if !row.Status.IsValid() {
			return domain.ErrInvalidUploadTaskData
		}
		switch row.Status {
		case domain.UploadStatusUploading, domain.UploadStatusCompleting, domain.UploadStatusVerifying, domain.UploadStatusCompleted:
			if row.StorageUploadID == nil {
				return domain.ErrInvalidUploadTaskData
			}
		}

		// 3. 汇总绑定、宽限期和任务状态，等待事务结束再交付。
		facts, err := row.cleanupFactsFor(session, gracePeriod)
		if err != nil {
			return err
		}
		check = domain.MultipartUploadSessionLockedCheck{TaskStatus: row.Status, Facts: facts}
		return nil
	})
	if err != nil {
		return domain.MultipartUploadSessionLockedCheck{}, err
	}
	return check, nil
}
