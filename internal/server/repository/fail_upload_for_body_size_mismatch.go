package repository

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// FailUploadForBodySizeMismatch 终结本次读回确认大小不符的校验任务。
// 调用方传入 VerifyUploadObject 的原始错误及本次读回使用的快照。
func (store *Store) FailUploadForBodySizeMismatch(ctx context.Context, task *domain.UploadTask, staged *domain.StoredObject, verificationError error) error {
	// 1. 核对本次读回得到的大小不符信息。
	if ctx == nil {
		return fmt.Errorf("终结大小不匹配的上传需要上下文")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// 只接收原始大小错误，附带读取、关闭或取消错误时保留任务供后续处理。
	mismatch, ok := verificationError.(*domain.UploadVerificationSizeMismatch)
	if !ok || mismatch == nil || task == nil ||
		mismatch.TaskID != task.ID || mismatch.UserID != task.UserID || mismatch.StagedObjectID != task.StagedObjectID ||
		mismatch.LeaseVersion != task.LeaseVersion || mismatch.DeclaredSizeBytes != task.FileSize {
		return domain.ErrInvalidUploadTaskData
	}

	// 2. 锁内复核声明、暂存对象版本、校验租约和 Multipart 原会话。
	return store.withUploadVerificationResultTransaction(ctx, task, staged, mismatch.ObservedSizeBytes, nil, false,
		func(txContext context.Context, txStore *Store, current *domain.UploadTask, _ *domain.StoredObject) error {
			// 3. 写入 BODY_SIZE_MISMATCH，释放预占并开始回收计时。
			return txStore.failVerifyingUpload(txContext, current, domain.UploadBodySizeMismatchCode, domain.ErrUploadBodySizeMismatch.Error())
		})
}
