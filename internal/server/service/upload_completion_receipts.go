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
	if !matchesClaimedUploadPartTarget(loaded, claimed) {
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

// matchesClaimedUploadPartTarget 检查目标保留本次领取的任务定位及计划。
// 调用前已复核 claimed 含有会话 ID 和完整计划。
func matchesClaimedUploadPartTarget(target *UploadPartSigningTarget, claimed *domain.UploadTask) bool {
	return target != nil && target.TaskID == claimed.ID && target.UserID == claimed.UserID &&
		target.StagedObjectID == claimed.StagedObjectID && target.LeaseVersion == claimed.LeaseVersion &&
		target.ObjectKey != "" && target.UploadID == *claimed.StorageUploadID && domain.ValidMultipartUploadID(target.UploadID) &&
		target.FileSizeBytes == claimed.FileSize && target.PartSizeBytes == *claimed.PartSize && target.PartCount == *claimed.PartCount &&
		target.ExpiresAt.Equal(claimed.ExpiresAt) && domain.ValidMultipartUploadLayout(target.FileSizeBytes, target.PartSizeBytes, target.PartCount)
}
