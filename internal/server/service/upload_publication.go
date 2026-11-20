package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadPublicationTransaction 汇集同一个发布事务中的读取与写入。
type UploadPublicationTransaction interface {
	FileEntryPathReader
	FailUploadForUnavailableTarget(ctx context.Context, task *domain.UploadTask) error
	FindUserActiveFileEntryByName(ctx context.Context, userID, parentID uint64, nameKey []byte) (*domain.FileEntry, error)
	SelectUploadPublicationObject(ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject, digest domain.UploadContentDigest) (*domain.StoredObject, error)
	FindUploadSourceDevice(ctx context.Context, task *domain.UploadTask) (*domain.UserDevice, error)
	CreatePublishedFileEntry(ctx context.Context, task *domain.UploadTask, input domain.CreatePublishedFileEntryInput) (*domain.FileEntry, error)
	SettlePublishedUploadQuota(ctx context.Context, task *domain.UploadTask) error
	CompletePublishedUpload(ctx context.Context, task *domain.UploadTask, objectID uint64, entry *domain.FileEntry) (*domain.UploadTask, error)
	ClearPublishedObjectGCTimer(ctx context.Context, objectID, entryID uint64) (*domain.StoredObject, error)
	MarkDeduplicatedUploadObjectOrphaned(ctx context.Context, taskID uint64) (*domain.StoredObject, error)
}

// UploadPublicationService 将单文件校验结果写入同一个发布事务。
type UploadPublicationService struct {
	// withTransaction 复核当前任务及租约，执行一次回调，确认提交后返回 nil。
	withTransaction func(context.Context, *domain.UploadTask, *domain.StoredObject, domain.UploadContentDigest,
		func(context.Context, UploadPublicationTransaction, *domain.UploadTask, *domain.StoredObject) error) error
}

// NewUploadPublicationService 绑定后台写回复核事务。
func NewUploadPublicationService(
	withTransaction func(context.Context, *domain.UploadTask, *domain.StoredObject, domain.UploadContentDigest,
		func(context.Context, UploadPublicationTransaction, *domain.UploadTask, *domain.StoredObject) error) error,
) *UploadPublicationService {
	// 1. 确认提供写回复核事务，并保存给发布入口使用。
	if withTransaction == nil {
		panic("上传发布服务需要写回复核事务")
	}
	return &UploadPublicationService{withTransaction: withTransaction}
}

// Publish 使用事务外已读回校验的任务、暂存对象和实测摘要发布文件。
// 对象、文件、配额、完成结果及计时由外层事务一起确认。
// 目标不可用时完成失败收尾，提交后返回 ErrUploadTargetUnavailable。
func (publication *UploadPublicationService) Publish(
	ctx context.Context,
	verifiedTask *domain.UploadTask,
	verifiedObject *domain.StoredObject,
	digest domain.UploadContentDigest,
) (*domain.UploadTask, error) {
	// 1. 接收校验结果并进入写回复核事务。
	if ctx == nil {
		return nil, fmt.Errorf("发布上传需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if verifiedTask == nil || verifiedTask.ID == 0 || verifiedTask.UserID == 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	taskID, userID := verifiedTask.ID, verifiedTask.UserID

	var prepared *domain.UploadTask
	var targetUnavailable bool
	err := publication.withTransaction(ctx, verifiedTask, verifiedObject, digest, func(txContext context.Context, txn UploadPublicationTransaction, task *domain.UploadTask, staged *domain.StoredObject) error {
		if txContext == nil || txn == nil {
			return fmt.Errorf("上传发布回调需要事务上下文和写入能力")
		}
		if task == nil || task.ID != taskID || task.UserID != userID {
			return domain.ErrInvalidUploadTaskData
		}

		// 2. 复核目标目录，选定对象、文件名和来源设备。
		if _, err := ValidateUploadPublicationTarget(txContext, task, txn); err != nil {
			if !errors.Is(err, domain.ErrUploadTargetUnavailable) {
				return err
			}
			if err := txn.FailUploadForUnavailableTarget(txContext, task); err != nil {
				return err
			}
			// 回调成功才能提交失败状态。待外层提交后再向调用方交付业务错误。
			targetUnavailable = true
			return nil
		}
		selected, err := txn.SelectUploadPublicationObject(txContext, task, staged, digest)
		if err != nil {
			return err
		}
		name, err := ResolveUploadPublicationName(txContext, userID, task.TargetParentID, task.RequestedName, txn.FindUserActiveFileEntryByName)
		if err != nil {
			return err
		}
		source, err := txn.FindUploadSourceDevice(txContext, task)
		if err != nil {
			return err
		}
		input, err := BuildPublishedFileEntryInput(task, selected, name, source)
		if err != nil {
			return err
		}

		// 3. 将校验得到的可选 MIME 随文件目录项写入，再结算配额并记录完成。
		if digest.DetectedMIMEType != "" {
			input.MIMEType = &digest.DetectedMIMEType
		}
		entry, err := txn.CreatePublishedFileEntry(txContext, task, *input)
		if err != nil {
			return err
		}
		if entry == nil || entry.ID == 0 {
			return domain.ErrInvalidFileEntryData
		}
		if err := txn.SettlePublishedUploadQuota(txContext, task); err != nil {
			return err
		}
		completed, err := txn.CompletePublishedUpload(txContext, task, selected.ID, entry)
		if err != nil {
			return err
		}
		if completed == nil || completed.ID != taskID || completed.UserID != userID || completed.Status != domain.UploadStatusCompleted ||
			completed.PublishedObjectID == nil || *completed.PublishedObjectID != selected.ID ||
			completed.ResultEntryID == nil || *completed.ResultEntryID != entry.ID {
			return domain.ErrInvalidUploadTaskData
		}

		// 4. 清除发布对象的计时，并处理冗余暂存副本。
		if _, err := txn.ClearPublishedObjectGCTimer(txContext, selected.ID, entry.ID); err != nil {
			return err
		}
		if selected.ID != task.StagedObjectID {
			if _, err := txn.MarkDeduplicatedUploadObjectOrphaned(txContext, taskID); err != nil {
				return err
			}
		}
		prepared = copyUploadTask(completed)
		return nil
	})

	// 5. 等待事务提交后返回完成任务。
	if err != nil {
		return nil, err
	}
	if targetUnavailable {
		return nil, domain.ErrUploadTargetUnavailable
	}
	if prepared == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return prepared, nil
}
