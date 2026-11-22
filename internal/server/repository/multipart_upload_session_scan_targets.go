package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

const maxMultipartUploadSessionScanTargets = 100

// ListMultipartUploadSessionScanTargets 只读分页取得当前存储配置下的 Multipart 原暂存对象。
// 已结束任务仍可能留下其他会话，扫描保留其原键。调用方应继续按会话复核。
func (store *Store) ListMultipartUploadSessionScanTargets(
	ctx context.Context, provider, bucket string, afterObjectID uint64, limit int,
) (*domain.MultipartUploadSessionScanPage, error) {
	// 1. 校验存储范围、游标和页大小。对象 ID 为零表示从头扫描。
	if store == nil || store.database == nil || ctx == nil || limit < 1 || limit > maxMultipartUploadSessionScanTargets {
		return nil, fmt.Errorf("扫描分片会话对象需要数据入口、上下文和 1～%d 条的批量大小", maxMultipartUploadSessionScanTargets)
	}
	if _, inTransaction := store.database.Statement.ConnPool.(gorm.TxCommitter); inTransaction {
		return nil, fmt.Errorf("跨用户扫描分片会话对象须使用事务外的数据入口")
	}
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if !location.IsValid() || !validMultipartMergeObjectText(provider) || !validMultipartMergeObjectText(bucket) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, err
	}

	// 2. 按归属关联 Multipart 任务和原暂存对象，多读一条判断是否还有下一页。
	rows := make([]domain.MultipartUploadSessionScanTarget, 0, limit+1)
	queryError := store.database.WithContext(queryContext).Table("stored_objects AS original").
		Select("original.id AS object_id, original.owner_user_id AS user_id, task.id AS task_id, original.object_key").
		Joins("JOIN upload_tasks AS task ON task.staged_object_id = original.id AND task.user_id = original.owner_user_id").
		Where("original.id > ? AND task.upload_mode = 'MULTIPART'", afterObjectID).
		Where("CAST(original.provider AS BINARY) = ? AND CAST(original.bucket AS BINARY) = ?", provider, bucket).
		Order("original.id ASC").Limit(limit + 1).Find(&rows).Error
	if err := queryContext.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("扫描分片会话对象失败: %w", queryError)
	}
	if len(rows) > limit+1 {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 核对完整键与账号。游标跨过已读记录，跳过的键不会阻塞扫描。
	page := &domain.MultipartUploadSessionScanPage{Targets: make([]domain.MultipartUploadSessionScanTarget, 0, min(len(rows), limit))}
	previousID := afterObjectID
	for index, row := range rows {
		if row.ObjectID <= previousID || row.UserID == 0 || row.TaskID == 0 {
			return nil, domain.ErrInvalidStoredObjectData
		}
		if index == limit {
			page.NextAfterObjectID = previousID
			break
		}
		previousID = row.ObjectID
		if owner, valid := domain.ParseUploadObjectKey(row.ObjectKey); valid && owner == row.UserID {
			page.Targets = append(page.Targets, row)
		}
	}
	return page, nil
}
