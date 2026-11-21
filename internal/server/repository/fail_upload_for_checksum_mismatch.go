package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// FailUploadForChecksumMismatch 终结本次完整读回后确认摘要不符的任务。
// 调用方传入 VerifyUploadObject 的原始错误及本次读回使用的快照。
func (store *Store) FailUploadForChecksumMismatch(ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject, verificationError error) error {
	// 1. 确认失败来自本次完整读回的摘要比较。
	if ctx == nil {
		return fmt.Errorf("终结摘要不匹配的上传需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// 直接匹配错误类型，合并了关闭故障或取消的结果不能终结任务。
	mismatch, ok := verificationError.(*domain.UploadVerificationChecksumMismatch)
	if !ok || mismatch == nil || task == nil || task.ExpectedSHA256 == nil ||
		mismatch.TaskID != task.ID || mismatch.UserID != task.UserID || mismatch.StagedObjectID != task.StagedObjectID ||
		mismatch.LeaseVersion != task.LeaseVersion || mismatch.ExpectedSHA256 != *task.ExpectedSHA256 {
		return domain.ErrInvalidUploadTaskData
	}

	// 2. 锁内复核声明、租约、暂存对象版本和 Multipart 原会话。
	return store.withUploadVerificationResultTransaction(ctx, task, staged, mismatch.Actual.SizeBytes, &mismatch.Actual.SHA256, true,
		func(txContext context.Context, txStore *Store, current *domain.UploadTask, _ *domain.StoredObject) error {
			// 3. 复用失败收尾，原子写入任务、配额和回收计时。
			return txStore.failVerifyingUpload(txContext, current, domain.UploadChecksumMismatchCode, domain.ErrUploadChecksumMismatch.Error())
		})
}
