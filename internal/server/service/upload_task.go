package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadTaskService 为已认证用户读取指定上传任务的内部事实。
type UploadTaskService struct {
	// findTask 绑定 Repository.FindUserUploadTask，按用户和任务范围读取。
	findTask func(context.Context, uint64, uint64) (*domain.UploadTask, error)
}

// NewUploadTaskService 绑定任务读取方法，缺少依赖属于装配错误。
func NewUploadTaskService(findTask func(context.Context, uint64, uint64) (*domain.UploadTask, error)) *UploadTaskService {
	// 1. 检查并保存任务读取依赖。
	if findTask == nil {
		panic("上传任务服务需要任务查询方法")
	}
	return &UploadTaskService{findTask: findTask}
}

// Get 为可信 Actor 读取用户所属任务，保留任务的来源设备及生命周期事实。
// Repository 负责记录完整性检查，服务检查交付范围并复制全部可空字段。
func (taskService *UploadTaskService) Get(ctx context.Context, actor domain.Actor, taskID uint64) (*domain.UploadTask, error) {
	// 1. 检查完整认证身份、任务 ID 和请求上下文。
	if ctx == nil || actor.UserID == 0 || actor.SessionID == 0 || actor.DeviceID == 0 || taskID == 0 {
		return nil, fmt.Errorf("读取上传任务需要上下文、完整认证身份和非零任务 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 按 Actor 的用户范围读取任务，处理取消和查询错误。
	task, queryError := taskService.findTask(ctx, actor.UserID, taskID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if queryError != nil {
		return nil, fmt.Errorf("读取上传任务失败: %w", queryError)
	}

	// 3. 核对任务标识、归属、来源设备及状态。
	if task == nil || task.ID != taskID || task.UserID != actor.UserID || task.DeviceID == 0 || !task.Status.IsValid() {
		return nil, domain.ErrInvalidUploadTaskData
	}

	// 4. 复制摘要与全部可空字段。
	result := copyUploadTask(task)

	// 5. 复查取消，返回独立任务结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// copyUploadTask 复制非空任务及全部可空字段，供查询和接收准备共同使用。
func copyUploadTask(task *domain.UploadTask) *domain.UploadTask {
	// 1. 按值复制记录，再独立持有摘要与可空标量。
	result := *task
	result.StorageUploadID = copyOptionalValue(task.StorageUploadID)
	result.ExpectedSHA256 = copyOptionalValue(task.ExpectedSHA256)
	result.PartSize = copyOptionalValue(task.PartSize)
	result.PartCount = copyOptionalValue(task.PartCount)
	result.PublishedObjectID = copyOptionalValue(task.PublishedObjectID)
	result.ResolvedName = copyOptionalValue(task.ResolvedName)
	result.ResultEntryID = copyOptionalValue(task.ResultEntryID)
	result.LeaseOwner = copyOptionalValue(task.LeaseOwner)
	result.LeaseUntil = copyOptionalValue(task.LeaseUntil)
	result.NextAttemptAt = copyOptionalValue(task.NextAttemptAt)
	result.LastErrorCode = copyOptionalValue(task.LastErrorCode)
	result.LastErrorMessage = copyOptionalValue(task.LastErrorMessage)
	result.CompletedAt = copyOptionalValue(task.CompletedAt)
	return &result
}
