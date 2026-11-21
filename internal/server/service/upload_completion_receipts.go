package service

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadCompletionReceiptService 核对已领取完成租约的任务与存储回执。
type UploadCompletionReceiptService struct {
	getTarget func(context.Context, domain.Actor, *domain.UploadTask) (*UploadPartSigningTarget, error)
	listParts func(context.Context, string, string) ([]domain.UploadPartReceipt, error)
}

// NewUploadCompletionReceiptService 绑定持租约的目标查询及完整分页读取。
func NewUploadCompletionReceiptService(
	getTarget func(context.Context, domain.Actor, *domain.UploadTask) (*UploadPartSigningTarget, error),
	listParts func(context.Context, string, string) ([]domain.UploadPartReceipt, error),
) *UploadCompletionReceiptService {
	if getTarget == nil || listParts == nil {
		panic("分片完成核对需要持租约的目标查询及完整分页读取方法")
	}
	return &UploadCompletionReceiptService{getTarget: getTarget, listParts: listParts}
}

// Verify 返回可用于完成快照的完整回执。调用方负责处理租约释放和完成快照提交。
func (receiptService *UploadCompletionReceiptService) Verify(
	ctx context.Context, actor domain.Actor, claimed *domain.UploadTask, claims []domain.UploadPartCompletionClaim,
) ([]domain.UploadPartReceipt, error) {
	// 1. 读取并保存本轮租约对应的存储目标。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || claimed == nil || claimed.ID == 0 ||
		claimed.LeaseVersion == 0 || claimed.LeaseOwner == nil || claimed.LeaseUntil == nil ||
		claimed.StagedObjectID == 0 || claimed.StorageUploadID == nil ||
		claimed.PartSize == nil || claimed.PartCount == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if claimed.UserID != actor.UserID {
		return nil, domain.ErrUploadTaskNotFound
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	loaded, err := receiptService.getTarget(ctx, actor, claimed)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, err
	}
	if loaded == nil || loaded.TaskID != claimed.ID || loaded.UserID != actor.UserID ||
		loaded.StagedObjectID != claimed.StagedObjectID || loaded.LeaseVersion != claimed.LeaseVersion ||
		loaded.ObjectKey == "" || loaded.UploadID != *claimed.StorageUploadID || !domain.ValidMultipartUploadID(loaded.UploadID) ||
		loaded.FileSizeBytes != claimed.FileSize || loaded.PartSizeBytes != *claimed.PartSize || loaded.PartCount != *claimed.PartCount ||
		!loaded.ExpiresAt.Equal(claimed.ExpiresAt) || !domain.ValidMultipartUploadLayout(loaded.FileSizeBytes, loaded.PartSizeBytes, loaded.PartCount) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	target := *loaded

	// 2. 读完 ListParts 分页，按固定计划逐片核对。
	receipts, listError := receiptService.listParts(ctx, target.ObjectKey, target.UploadID)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if listError != nil && !errors.Is(listError, domain.ErrUploadPartSessionMissing) {
		return nil, domain.ErrUploadStorageUnavailable
	}
	var matched []domain.UploadPartReceipt
	var matchError error
	if listError == nil {
		matched, matchError = MatchUploadCompletionParts(target.FileSizeBytes, target.PartSizeBytes, target.PartCount, claims, receipts)
	}

	// 3. 复查租约和目标，交付完整回执或返回错误。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, err := receiptService.getTarget(ctx, actor, claimed)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if current.LeaseVersion != target.LeaseVersion {
		return nil, domain.ErrUploadTaskBusy
	}
	if current.TaskID != target.TaskID || current.UserID != target.UserID || current.StagedObjectID != target.StagedObjectID ||
		current.ObjectKey != target.ObjectKey || current.UploadID != target.UploadID || current.FileSizeBytes != target.FileSizeBytes ||
		current.PartSizeBytes != target.PartSizeBytes || current.PartCount != target.PartCount || !current.ExpiresAt.Equal(target.ExpiresAt) {
		return nil, domain.ErrUploadTaskNotReady
	}
	if listError != nil {
		return nil, domain.ErrUploadStorageUnavailable
	}
	return matched, matchError
}
