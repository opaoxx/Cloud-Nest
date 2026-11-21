package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

// ReadMultipartCompletionReceipts 返回当前合并租约对应的完整冻结回执。
// claimed 来自 ClaimMultipartUploadCompletionLease。账号和任务锁保护读取期间的任务与回执，调用方后续写回仍须核对租约。
func (store *Store) ReadMultipartCompletionReceipts(ctx context.Context, claimed *domain.UploadTask) ([]domain.UploadPartReceipt, error) {
	// 1. 校验租约快照，锁定所属账号并复查任务和租约。
	if store == nil || store.database == nil || ctx == nil {
		return nil, fmt.Errorf("读取分片完成快照需要数据入口和上下文")
	}
	if !validMultipartMergeClaim(claimed) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	var receipts []domain.UploadPartReceipt
	err := store.withActiveUserTransaction(ctx, claimed.UserID, func(txContext context.Context, txStore *Store) error {
		current, err := txStore.lockMultipartMergeTask(txContext, claimed)
		if err != nil {
			return err
		}

		// 2. 按分片编号读取回执，多读至多一条用于识别超出计划的记录。
		partCount := int(*current.PartCount)
		rows := make([]models.UploadPart, 0, partCount)
		queryError := txStore.database.WithContext(txContext).
			Select("task_id", "part_number", "etag", "size", "checksum_algorithm", "checksum_value").
			Where("task_id = ?", current.ID).Order("part_number ASC").Limit(partCount + 1).Find(&rows).Error
		if err := txContext.Err(); err != nil {
			return err
		}
		if queryError != nil {
			return fmt.Errorf("查询分片完成快照失败: %w", queryError)
		}
		if len(rows) != partCount {
			return domain.ErrInvalidUploadTaskData
		}
		receipts = make([]domain.UploadPartReceipt, partCount)
		for index, row := range rows {
			number := uint32(index + 1)
			size, err := domain.ExpectedMultipartUploadPartSize(current.FileSize, *current.PartSize, *current.PartCount, number)
			if err != nil || row.TaskID != current.ID || row.PartNumber != number || row.SizeBytes != size ||
				!domain.ValidUploadPartETag(row.ETag) || row.ChecksumAlgorithm == nil || *row.ChecksumAlgorithm != "SHA256" ||
				row.ChecksumValue == nil || !domain.ValidUploadPartChecksumSHA256(*row.ChecksumValue) {
				return domain.ErrInvalidUploadTaskData
			}
			receipts[index] = domain.UploadPartReceipt{PartNumber: number, SizeBytes: size, ETag: row.ETag, ChecksumSHA256: *row.ChecksumValue}
		}

		// 3. 再用数据库时钟检查期限，通过后返回完整回执。
		return txStore.checkMultipartCompletionDeadlines(txContext, current)
	})
	if err != nil {
		return nil, err
	}
	return receipts, nil
}

// checkMultipartCompletionDeadlines 使用当前事务的时钟判断任务和已复核租约是否仍可用。
func (store *Store) checkMultipartCompletionDeadlines(ctx context.Context, task *domain.UploadTask) error {
	var now time.Time
	queryError := store.database.WithContext(ctx).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&now)
	if err := ctx.Err(); err != nil {
		return err
	}
	if queryError != nil {
		return fmt.Errorf("读取分片合并租约的数据库时间失败: %w", queryError)
	}
	if now.Year() < 1000 || now.Year() > 9999 || now.Nanosecond()%1000 != 0 {
		return domain.ErrInvalidUploadTaskData
	}
	if !now.Before(task.ExpiresAt) {
		return domain.ErrUploadTaskNotReady
	}
	if !now.Before(*task.LeaseUntil) {
		return domain.ErrUploadTaskBusy
	}
	return nil
}
