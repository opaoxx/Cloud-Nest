package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadPartReceiptService 核对当前用户的任务和存储端已收到的分片。
type UploadPartReceiptService struct {
	getTarget func(context.Context, domain.Actor, uint64) (*UploadPartSigningTarget, error)
	listParts func(context.Context, string, string) ([]domain.UploadPartReceipt, error)
}

// NewUploadPartReceiptService 绑定只读目标查询及完整分页读取。
func NewUploadPartReceiptService(
	getTarget func(context.Context, domain.Actor, uint64) (*UploadPartSigningTarget, error),
	listParts func(context.Context, string, string) ([]domain.UploadPartReceipt, error),
) *UploadPartReceiptService {
	if getTarget == nil || listParts == nil {
		panic("分片回执核对需要只读目标查询和完整分页读取方法")
	}
	return &UploadPartReceiptService{getTarget: getTarget, listParts: listParts}
}

// List 读取本轮可用回执，交付前再次确认目标可读且绑定未变化。
func (receiptService *UploadPartReceiptService) List(ctx context.Context, actor domain.Actor, taskID uint64) (*domain.UploadPartReceiptList, error) {
	// 1. 按认证账号读取目标，保存本轮使用的对象、会话和分片计划。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || taskID == 0 {
		return nil, fmt.Errorf("读取分片回执需要上下文、完整认证身份和非零任务 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	loaded, err := receiptService.getTarget(ctx, actor, taskID)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, err
	}
	if loaded == nil || loaded.TaskID != taskID || loaded.UserID != actor.UserID || loaded.StagedObjectID == 0 ||
		loaded.ObjectKey == "" || !domain.ValidMultipartUploadID(loaded.UploadID) ||
		!domain.ValidMultipartUploadLayout(loaded.FileSizeBytes, loaded.PartSizeBytes, loaded.PartCount) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	target := *loaded

	// 2. 完整读取并筛选回执。存储会话消失时先复查任务。
	receipts, listError := receiptService.listParts(ctx, target.ObjectKey, target.UploadID)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if listError != nil && !errors.Is(listError, domain.ErrUploadPartSessionMissing) {
		return nil, domain.ErrUploadStorageUnavailable
	}
	var available []domain.UploadPartReceipt
	if listError == nil {
		available, err = FilterUploadPartReceipts(target.FileSizeBytes, target.PartSizeBytes, target.PartCount, receipts)
		if err != nil {
			return nil, err
		}
	}

	// 3. 重读可读目标，确认仍对应查询时的绑定和分片计划，再交付本轮回执。
	current, err := receiptService.getTarget(ctx, actor, taskID)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if current.TaskID != target.TaskID || current.UserID != target.UserID || current.StagedObjectID != target.StagedObjectID ||
		current.ObjectKey != target.ObjectKey || current.UploadID != target.UploadID || current.FileSizeBytes != target.FileSizeBytes ||
		current.PartSizeBytes != target.PartSizeBytes || current.PartCount != target.PartCount {
		return nil, domain.ErrUploadTaskNotReady
	}
	if listError != nil {
		return nil, domain.ErrUploadStorageUnavailable
	}
	return &domain.UploadPartReceiptList{PartSizeBytes: target.PartSizeBytes, PartCount: target.PartCount, Parts: available}, nil
}
