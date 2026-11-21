package repository

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
)

// FindMultipartUploadCancellationTarget 按已提交的取消收尾租约只读查询原存储目标。
// provider 和 bucket 来自实际存储客户端配置。后续写回仍须在事务中复核任务和存储绑定。
func (store *Store) FindMultipartUploadCancellationTarget(
	ctx context.Context, claimed *domain.UploadTask, provider, bucket string,
) (*domain.MultipartUploadCancellationTarget, error) {
	// 1. 在事务外读取当前任务，使用同条查询的数据库时间核对取消租约。
	if store == nil || store.database == nil || ctx == nil {
		return nil, fmt.Errorf("查询分片取消目标需要数据入口和上下文")
	}
	if _, nested := store.database.Statement.ConnPool.(gorm.TxCommitter); nested {
		return nil, fmt.Errorf("分片取消目标需要在事务外读取最新租约")
	}
	if claimed == nil || claimed.ID == 0 || claimed.UserID == 0 || claimed.StagedObjectID == 0 ||
		claimed.Status != domain.UploadStatusCancelling || claimed.UploadMode != "MULTIPART" ||
		claimed.LeaseOwner == nil || *claimed.LeaseOwner == "" || claimed.LeaseUntil == nil ||
		claimed.LeaseVersion == 0 || !validPendingMultipartUploadCancellationTask(claimed) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	current, err := store.readMultipartUploadCancellationLease(queryContext, claimed)
	if err != nil {
		return nil, err
	}

	// 2. 按同一账号读取暂存对象，核对存储位置和未发布状态。
	object, err := store.FindUserStoredObject(queryContext, current.UserID, current.StagedObjectID)
	if err != nil {
		return nil, err
	}
	location := domain.CreateStagedObjectInput{Provider: object.Provider, Bucket: object.Bucket, ObjectKey: object.ObjectKey}
	if !validMultipartUploadCancellationObject(current, object) || object.Provider != provider || object.Bucket != bucket ||
		!location.IsValid() || strings.TrimSpace(object.ObjectKey) == "" || strings.ContainsFunc(object.ObjectKey, unicode.IsControl) {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 再读任务确认租约仍有效，返回原对象键和可空会话。
	rechecked, err := store.readMultipartUploadCancellationLease(queryContext, claimed)
	if err != nil {
		return nil, err
	}
	return &domain.MultipartUploadCancellationTarget{ObjectKey: object.ObjectKey, UploadID: rechecked.StorageUploadID}, nil
}

func (store *Store) readMultipartUploadCancellationLease(ctx context.Context, claimed *domain.UploadTask) (*domain.UploadTask, error) {
	current, databaseTime, err := store.findUserUploadTaskWithDatabaseTime(ctx, claimed.UserID, claimed.ID)
	if err != nil {
		return nil, err
	}
	if current.Status != domain.UploadStatusCancelling || current.UploadMode != "MULTIPART" {
		return nil, domain.ErrUploadTaskNotReady
	}
	if current.StagedObjectID != claimed.StagedObjectID || current.LeaseVersion != claimed.LeaseVersion ||
		!sameUploadStorageText(current.LeaseOwner, claimed.LeaseOwner) || current.LeaseUntil == nil ||
		!current.LeaseUntil.Equal(*claimed.LeaseUntil) || !databaseTime.Before(*current.LeaseUntil) ||
		(current.NextAttemptAt != nil && databaseTime.Before(*current.NextAttemptAt)) ||
		!sameUploadStorageText(current.StorageUploadID, claimed.StorageUploadID) {
		return nil, domain.ErrUploadTaskBusy
	}
	if !validPendingMultipartUploadCancellationTask(current) || !sameMultipartUploadDeclaration(claimed, current) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return current, nil
}
