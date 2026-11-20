package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// PurgeProgressReader 在同一只读快照提供进度目标和完整子树。
type PurgeProgressReader interface {
	PurgeProgressTargetReader
	ListUserPurgePreviewSubtree(ctx context.Context, userID, entryID uint64) ([]domain.PurgePreviewSubtreeEntry, error)
}

// PurgeProgressService 组合读取进度目标及剩余项所需的查询。
type PurgeProgressService struct {
	// withSnapshot 同步执行一次回调，并在快照成功结束后返回 nil。
	withSnapshot func(context.Context, func(context.Context, PurgeProgressReader) error) error
}

// NewPurgeProgressService 绑定已有目录项只读快照入口。
func NewPurgeProgressService(withSnapshot func(context.Context, func(context.Context, PurgeProgressReader) error) error) *PurgeProgressService {
	// 1. 校验并保存快照依赖。
	if withSnapshot == nil {
		panic("永久清理进度服务需要只读快照方法")
	}
	return &PurgeProgressService{withSnapshot: withSnapshot}
}

// Get 查询当前认证用户的单项永久清理进度。
func (progressService *PurgeProgressService) Get(ctx context.Context, actor domain.Actor, entryID uint64) (*domain.PurgeProgress, error) {
	// 1. 校验请求输入，进入只读快照。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || entryID == 0 {
		return nil, fmt.Errorf("查询永久清理进度需要上下文、完整认证身份和非零目标 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var prepared *domain.PurgeProgress
	snapshotError := progressService.withSnapshot(ctx, func(snapshotContext context.Context, reader PurgeProgressReader) error {
		// 2. 读取并确认目标具备查询资格。
		target, err := ReadPurgeProgressTarget(snapshotContext, actor, entryID, reader)
		if err != nil {
			return err
		}

		// 3. 清理中则读取子树并计数，已完成则使用 0。
		progress := domain.PurgeProgress{EntryID: target.ID, Status: target.Status}
		if target.Status == "PURGING" {
			entries, readError := reader.ListUserPurgePreviewSubtree(snapshotContext, target.UserID, target.ID)
			if err := snapshotContext.Err(); err != nil {
				return err
			}
			if errors.Is(readError, domain.ErrFileEntryNotFound) {
				// 当前快照已经读到目标，子树读取再报告它缺失属于数据异常。
				return domain.ErrInvalidFileEntryData
			}
			if readError != nil {
				return fmt.Errorf("读取清理进度子树失败: %w", readError)
			}
			progress.RemainingCount, err = CountPurgeRemainingEntries(target.ID, entries)
			if err != nil {
				return err
			}
		}
		prepared = &progress
		return snapshotContext.Err()
	})

	// 4. 快照成功结束后交付状态和剩余项。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshotError != nil {
		return nil, fmt.Errorf("读取永久清理进度快照失败: %w", snapshotError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
