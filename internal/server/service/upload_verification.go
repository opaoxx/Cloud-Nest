package service

import (
	"context"
	"errors"
	"io"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadVerificationService 处理一次已领取租约的单文件校验。
type UploadVerificationService struct {
	// open 读取暂存对象所记录的存储版本。
	open func(context.Context, *domain.StoredObject) (io.ReadCloser, error)
	// publish 在写回复核通过后发布文件并返回已提交的任务。
	publish func(context.Context, *domain.UploadTask, *domain.StoredObject, domain.UploadContentDigest) (*domain.UploadTask, error)
	// failChecksumMismatch 与 failSizeMismatch 确认失败事务提交后返回 nil。
	failChecksumMismatch func(context.Context, *domain.UploadTask, *domain.StoredObject, error) error
	failSizeMismatch     func(context.Context, *domain.UploadTask, *domain.StoredObject, error) error
	// scheduleRetry 在当前租约下记录下次尝试时间，返回已提交的任务。
	scheduleRetry func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error)
}

// NewUploadVerificationService 绑定读回、发布、失败收尾与重试写回入口。
func NewUploadVerificationService(
	open func(context.Context, *domain.StoredObject) (io.ReadCloser, error),
	publish func(context.Context, *domain.UploadTask, *domain.StoredObject, domain.UploadContentDigest) (*domain.UploadTask, error),
	failChecksumMismatch func(context.Context, *domain.UploadTask, *domain.StoredObject, error) error,
	failSizeMismatch func(context.Context, *domain.UploadTask, *domain.StoredObject, error) error,
	scheduleRetry func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error),
) *UploadVerificationService {
	// 1. 复用各入口并检查装配是否完整。
	if open == nil || publish == nil || failChecksumMismatch == nil || failSizeMismatch == nil || scheduleRetry == nil {
		panic("上传校验处理需要对象读取、发布、失败收尾与重试写回方法")
	}
	return &UploadVerificationService{
		open: open, publish: publish,
		failChecksumMismatch: failChecksumMismatch, failSizeMismatch: failSizeMismatch,
		scheduleRetry: scheduleRetry,
	}
}

// VerifyAndPublish 读回一次暂存对象，返回已发布或已安排重试的任务。
// 大小或摘要失配收尾成功后返回原始校验错误，写回失败时返回写回错误。
func (verification *UploadVerificationService) VerifyAndPublish(ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject) (*domain.UploadTask, error) {
	// 1. 读回暂存对象，取得实测摘要或校验错误。
	digest, verificationError := VerifyUploadObject(ctx, task, staged, verification.open)
	return verification.writeVerificationResult(ctx, task, staged, digest, verificationError)
}

// VerifyMultipartAndPublish 为已领取的分片校验在对象读回期间续租。
// renew 同步续约同一租约，失败时原样返回其错误。
func (verification *UploadVerificationService) VerifyMultipartAndPublish(
	ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject,
	renew func(context.Context, *domain.UploadTask) error,
) (*domain.UploadTask, error) {
	// 1. 为分片读回建立可取消的上下文，启动定时续租。
	if ctx == nil || renew == nil {
		return nil, errors.New("校验分片上传需要上下文和续租方法")
	}
	if task == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.UploadMode != "MULTIPART" {
		return nil, domain.ErrUploadTaskNotReady
	}
	readContext, cancelRead := context.WithCancelCause(ctx)
	stopRenewal := startUploadLeaseRenewal(readContext, cancelRead, func(renewContext context.Context) error {
		return renew(renewContext, task)
	})
	defer func() {
		cancelRead(nil)
		_ = stopRenewal()
	}()

	// 2. 读回并关闭对象流，停止续租并等待在途事务。
	digest, verificationError := VerifyUploadObject(readContext, task, staged, verification.open)
	if err := stopRenewal(); err != nil {
		return nil, err
	}

	// 3. 续租正常结束后，用原调用上下文写回校验结果。
	return verification.writeVerificationResult(ctx, task, staged, digest, verificationError)
}

func (verification *UploadVerificationService) writeVerificationResult(
	ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject,
	digest *domain.UploadContentDigest, verificationError error,
) (*domain.UploadTask, error) {
	// 1. 校验通过后发布文件。
	if verificationError == nil {
		return verification.publish(ctx, task, staged, *digest)
	}

	// 2. 临时存储故障按已记录的重试次数安排下次尝试。
	if errors.Is(verificationError, domain.ErrUploadStorageUnavailable) || errors.Is(verificationError, context.DeadlineExceeded) {
		// 存储请求可能先超时。只有外层调用仍有效，才能写入重试记录。
		if contextError := ctx.Err(); contextError != nil {
			if !errors.Is(verificationError, contextError) {
				verificationError = errors.Join(verificationError, contextError)
			}
			return nil, verificationError
		}
		if errors.Is(verificationError, context.Canceled) {
			return nil, verificationError
		}
		return verification.scheduleRetry(ctx, task, uploadRetryDelaySeconds(task.AttemptCount))
	}

	// 3. 已确认的大小或摘要失配继续交给失败收尾事务。
	// 直接匹配原始类型，带有其他故障的错误不能作为失配收尾的证据。
	var finalize func(context.Context, *domain.UploadTask, *domain.StoredObject, error) error
	switch mismatch := verificationError.(type) {
	case *domain.UploadVerificationChecksumMismatch:
		if mismatch != nil {
			finalize = verification.failChecksumMismatch
		}
	case *domain.UploadVerificationSizeMismatch:
		if mismatch != nil {
			finalize = verification.failSizeMismatch
		}
	}
	if finalize == nil {
		return nil, verificationError
	}

	// 收尾事务提交后返回失配原因。
	if err := finalize(ctx, task, staged, verificationError); err != nil {
		return nil, err
	}
	return nil, verificationError
}
