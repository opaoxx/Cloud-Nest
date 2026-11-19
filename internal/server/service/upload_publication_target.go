package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ValidateUploadPublicationTarget 复核已领取校验任务的目标目录和全部祖先。
// 调用方提供写回复核回调中的任务、上下文和当前事务读取能力。
func ValidateUploadPublicationTarget(ctx context.Context, task *domain.UploadTask, reader FileEntryPathReader) (*domain.FileEntryPath, error) {
	// 1. 核对事务读取能力、任务及目标目录。
	if ctx == nil || reader == nil {
		return nil, fmt.Errorf("复核发布目标需要事务上下文和路径读取能力")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if task == nil || task.ID == 0 || task.UserID == 0 {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if task.TargetParentID == 0 {
		return nil, domain.ErrInvalidUploadParentID
	}
	if task.Status != domain.UploadStatusVerifying || task.UploadMode != "SINGLE" {
		return nil, domain.ErrUploadTaskNotReady
	}

	// 2. 按任务用户读取目标到根目录的完整路径。
	path, err := readFileEntryPath(ctx, reader, task.UserID, task.TargetParentID)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if errors.Is(err, domain.ErrFileEntryNotFound) {
		return nil, domain.ErrUploadTargetUnavailable
	}
	if err != nil {
		return nil, err
	}

	// 3. 确认目标是目录，且新增文件符合层级上限。
	// 根目录计入路径长度，路径长度也正好是新增文件的深度。
	if path.Entries[len(path.Entries)-1].EntryType != "DIR" || len(path.Entries) > domain.MaxFileEntryDepth {
		return nil, domain.ErrUploadTargetUnavailable
	}
	return path, nil
}
