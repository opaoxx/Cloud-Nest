package service

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CreateUploadReplacementTransaction 提供首次内容替换所需的锁读和三项写入。
type CreateUploadReplacementTransaction interface {
	UploadReplacementTargetLocker
	uploadStagingTransaction
	CreateUserUploadReplacementTask(ctx context.Context, stagedObjectID uint64, input domain.CreateUploadInput,
		plan domain.UploadPlan, entryID, expectedRevision uint64, expiresAt time.Time) (*domain.UploadTask, error)
}

// CreateUploadReplacementInTransaction 在调用方的已认证账号事务内创建内容替换任务。
// 调用方须将错误传给外层事务，并在确认提交后方可交付任务。
func CreateUploadReplacementInTransaction(ctx context.Context, actor domain.Actor, declaration UploadReplacementDeclaration,
	limits domain.UploadPlanLimits, provider, bucket string, transaction CreateUploadReplacementTransaction,
) (*domain.UploadTask, error) {
	// 1. 锁读目标文件，复核版本并计算上传计划。
	prepared, err := PrepareUploadReplacement(ctx, actor, declaration, limits, transaction)
	if err != nil {
		return nil, err
	}

	// 2. 按新内容全额预占空间，登记暂存对象。
	objectInput := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, SizeBytes: declaration.SizeBytes}
	objectID, err := reserveAndStageUpload(ctx, actor.UserID, objectInput, transaction, GenerateUploadObjectKey)
	if err != nil {
		return nil, err
	}

	// 3. 写入替换任务，核对初始状态与绑定，交由外层事务提交。
	input := domain.CreateUploadInput{
		ParentID: prepared.ParentID, Name: prepared.Name, SizeBytes: declaration.SizeBytes,
		ExpectedSHA256: &declaration.ExpectedSHA256,
	}
	taskInput := input
	taskInput.ExpectedSHA256 = copyOptionalValue(input.ExpectedSHA256)
	expiresAt := time.Now().UTC().Truncate(time.Microsecond).Add(uploadTaskLifetime)
	task, createError := transaction.CreateUserUploadReplacementTask(ctx, objectID, taskInput, prepared.Plan,
		declaration.EntryID, declaration.ExpectedRevision, expiresAt)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if createError != nil {
		return nil, fmt.Errorf("创建内容替换任务失败: %w", createError)
	}
	if err := validateCreatedUploadTask(task, actor, objectID, input, prepared.Plan, expiresAt); err != nil {
		return nil, err
	}
	if !task.HasValidReplacementState() || task.ReplacementEntryID == nil || *task.ReplacementEntryID != declaration.EntryID ||
		*task.ReplacementExpectedRevision != declaration.ExpectedRevision {
		return nil, domain.ErrInvalidUploadTaskData
	}
	return copyUploadTask(task), nil
}
