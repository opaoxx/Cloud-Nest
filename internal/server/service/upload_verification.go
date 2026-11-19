package service

import (
	"context"
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
}

// NewUploadVerificationService 绑定读回、发布和两个失败收尾入口。
func NewUploadVerificationService(
	open func(context.Context, *domain.StoredObject) (io.ReadCloser, error),
	publish func(context.Context, *domain.UploadTask, *domain.StoredObject, domain.UploadContentDigest) (*domain.UploadTask, error),
	failChecksumMismatch func(context.Context, *domain.UploadTask, *domain.StoredObject, error) error,
	failSizeMismatch func(context.Context, *domain.UploadTask, *domain.StoredObject, error) error,
) *UploadVerificationService {
	// 1. 复用各入口并检查装配是否完整。
	if open == nil || publish == nil || failChecksumMismatch == nil || failSizeMismatch == nil {
		panic("上传校验处理需要对象读取、发布与失败收尾方法")
	}
	return &UploadVerificationService{
		open: open, publish: publish,
		failChecksumMismatch: failChecksumMismatch, failSizeMismatch: failSizeMismatch,
	}
}

// VerifyAndPublish 读回一次暂存对象，将结果交给发布或失败收尾事务。
// 大小或摘要失配收尾成功后返回原始校验错误，写回失败时返回写回错误。
func (verification *UploadVerificationService) VerifyAndPublish(ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject) (*domain.UploadTask, error) {
	// 1. 读回暂存对象，取得实测摘要或校验错误。
	digest, verificationError := VerifyUploadObject(ctx, task, staged, verification.open)

	// 2. 读回校验通过后，调用发布事务。
	if verificationError == nil {
		return verification.publish(ctx, task, staged, *digest)
	}

	// 3. 识别可确认的失配，选择对应的失败收尾方法。
	// 直接匹配原始类型，带有读取、关闭或取消故障的错误原样返回。
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

	// 4. 等待收尾事务提交，再返回失配原因。
	if err := finalize(ctx, task, staged, verificationError); err != nil {
		return nil, err
	}
	return nil, verificationError
}
