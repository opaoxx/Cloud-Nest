package service

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// loadUploadMergeTarget 在访问存储前复制当前租约绑定的目标。
func loadUploadMergeTarget(
	ctx context.Context, claimed *domain.UploadTask, getTarget func(context.Context, *domain.UploadTask) (*UploadPartSigningTarget, error),
) (UploadPartSigningTarget, error) {
	if ctx == nil || claimed == nil || claimed.ID == 0 || claimed.UserID == 0 || claimed.StagedObjectID == 0 ||
		claimed.Status != domain.UploadStatusCompleting || claimed.UploadMode != "MULTIPART" ||
		claimed.LeaseVersion == 0 || claimed.LeaseOwner == nil || claimed.LeaseUntil == nil ||
		claimed.StorageUploadID == nil || claimed.PartSize == nil || claimed.PartCount == nil {
		return UploadPartSigningTarget{}, domain.ErrInvalidUploadTaskData
	}
	if err := ctx.Err(); err != nil {
		return UploadPartSigningTarget{}, err
	}
	loaded, err := getTarget(ctx, claimed)
	if contextError := ctx.Err(); contextError != nil {
		return UploadPartSigningTarget{}, contextError
	}
	if err != nil {
		return UploadPartSigningTarget{}, err
	}
	if !matchesClaimedUploadPartTarget(loaded, claimed) {
		return UploadPartSigningTarget{}, domain.ErrInvalidUploadTaskData
	}
	target := *loaded
	target.ExpiresAt = target.ExpiresAt.UTC()
	return target, nil
}

// recheckUploadMergeTarget 使用重新读取的租约和位置复核存储查询的原目标。
func recheckUploadMergeTarget(
	ctx context.Context, claimed *domain.UploadTask, target UploadPartSigningTarget,
	getTarget func(context.Context, *domain.UploadTask) (*UploadPartSigningTarget, error),
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := getTarget(ctx, claimed)
	if contextError := ctx.Err(); contextError != nil {
		return contextError
	}
	if err != nil {
		return err
	}
	if current == nil {
		return domain.ErrInvalidUploadTaskData
	}
	if current.LeaseVersion != target.LeaseVersion {
		return domain.ErrUploadTaskBusy
	}
	checked := *current
	checked.ExpiresAt = checked.ExpiresAt.UTC()
	if checked != target {
		return domain.ErrUploadTaskNotReady
	}
	return nil
}
