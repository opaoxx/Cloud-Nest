package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const multipartCompletionReleaseTimeout = 5 * time.Second

// MultipartUploadCompletionService 组装分片上传完成的应用流程。
type MultipartUploadCompletionService struct {
	findTask func(context.Context, uint64, uint64) (*domain.UploadTask, time.Time, error)
	claim    func(context.Context, domain.Actor, *domain.UploadTask, string,
		func(*domain.UploadTask, uint64, uint64, time.Time) (domain.UploadStatus, error)) (*domain.UploadTask, error)
	verify  func(context.Context, domain.Actor, *domain.UploadTask, []domain.UploadPartCompletionClaim) ([]domain.UploadPartReceipt, error)
	commit  func(context.Context, domain.Actor, *domain.UploadTask, []domain.UploadPartReceipt) (*domain.UploadTask, error)
	release func(context.Context, domain.Actor, *domain.UploadTask) (bool, error)
}

// NewMultipartUploadCompletionService 绑定账号任务读取、核对和仓储事务。
func NewMultipartUploadCompletionService(
	findTask func(context.Context, uint64, uint64) (*domain.UploadTask, time.Time, error),
	claim func(context.Context, domain.Actor, *domain.UploadTask, string,
		func(*domain.UploadTask, uint64, uint64, time.Time) (domain.UploadStatus, error)) (*domain.UploadTask, error),
	verify func(context.Context, domain.Actor, *domain.UploadTask, []domain.UploadPartCompletionClaim) ([]domain.UploadPartReceipt, error),
	commit func(context.Context, domain.Actor, *domain.UploadTask, []domain.UploadPartReceipt) (*domain.UploadTask, error),
	release func(context.Context, domain.Actor, *domain.UploadTask) (bool, error),
) *MultipartUploadCompletionService {
	if findTask == nil || claim == nil || verify == nil || commit == nil || release == nil {
		panic("分片上传完成服务需要任务读取、领取、核对、提交和释放方法")
	}
	return &MultipartUploadCompletionService{findTask: findTask, claim: claim, verify: verify, commit: commit, release: release}
}

// Complete 受理新的 Multipart 完成请求或交付已受理任务。
// readClaims 只在可发起新核对时调用，早于租约领取。
func (completion *MultipartUploadCompletionService) Complete(
	ctx context.Context, actor domain.Actor, taskID uint64,
	readClaims func() ([]domain.UploadPartCompletionClaim, error),
) (result *domain.UploadTask, err error) {
	// 1. 按账号读取任务，区分新提交和已受理重试。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || taskID == 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	loaded, databaseTime, err := completion.findTask(ctx, actor.UserID, taskID)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, err
	}
	status, err := ResolveMultipartUploadCompletionStatus(loaded, actor.UserID, taskID, databaseTime)
	if err != nil {
		return nil, err
	}
	task := copyUploadTask(loaded)
	if status != domain.UploadStatusUploading {
		return task, nil
	}

	// 2. 读取回执声明，领取租约并完成存储核对。
	if readClaims == nil {
		return nil, &domain.UploadPartDeclarationError{Field: "parts", Message: "完成请求须提交分片回执"}
	}
	claims, err := readClaims()
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, err
	}
	if len(claims) < 1 || len(claims) > domain.MaxUploadPartCount {
		return nil, &domain.UploadPartDeclarationError{Field: "parts", Message: fmt.Sprintf("完成回执须包含 1～%d 项", domain.MaxUploadPartCount)}
	}
	ownerID, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("生成分片完成核对租约持有者失败: %w", err)
	}
	owner := ownerID.String()
	claimed, err := completion.claim(ctx, actor, task, owner, ResolveMultipartUploadCompletionStatus)
	if err != nil {
		return nil, err
	}
	if claimed == nil || claimed.ID != taskID || claimed.UserID != actor.UserID || claimed.StagedObjectID != task.StagedObjectID ||
		claimed.Status != domain.UploadStatusUploading || claimed.UploadMode != "MULTIPART" ||
		claimed.StorageUploadID == nil || *claimed.StorageUploadID != *task.StorageUploadID ||
		claimed.LeaseOwner == nil || *claimed.LeaseOwner != owner || claimed.LeaseUntil == nil ||
		claimed.LeaseVersion == 0 || claimed.LeaseVersion != task.LeaseVersion+1 {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 3. 核对通过后提交快照，失败时条件释放租约。
	defer func() {
		if err == nil {
			return
		}
		releaseContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), multipartCompletionReleaseTimeout)
		defer cancel()
		if _, releaseError := completion.release(releaseContext, actor, claimed); releaseError != nil {
			err = &domain.UploadCompletionReleaseError{Cause: err, ReleaseError: releaseError}
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	receipts, err := completion.verify(ctx, actor, claimed, claims)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, err
	}
	committed, err := completion.commit(ctx, actor, claimed, receipts)
	if err != nil {
		return nil, err
	}
	if committed == nil || committed.ID != taskID || committed.UserID != actor.UserID ||
		committed.StagedObjectID != claimed.StagedObjectID || committed.UploadMode != "MULTIPART" ||
		committed.Status != domain.UploadStatusCompleting || committed.LeaseVersion != claimed.LeaseVersion ||
		committed.LeaseOwner != nil || committed.LeaseUntil != nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return copyUploadTask(committed), nil
}
