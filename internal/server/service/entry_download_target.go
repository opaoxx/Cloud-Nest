package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// EntryDownloadTargetReader 提供同一快照中的路径与内容引用读取。
type EntryDownloadTargetReader interface {
	FileEntryPathReader
	// FindUserFileEntryContent 按用户和目标读取该目录项的物理对象引用。
	FindUserFileEntryContent(ctx context.Context, userID, entryID uint64) (*domain.FileEntryContent, error)
}

// EntryDownloadTargetService 读取可见文件指定版本的下载目标。
type EntryDownloadTargetService struct {
	// withSnapshot 同步执行读取，并在快照成功结束后返回 nil。
	withSnapshot func(context.Context, func(context.Context, EntryDownloadTargetReader) error) error
}

// NewEntryDownloadTargetService 绑定只读快照入口。
func NewEntryDownloadTargetService(withSnapshot func(context.Context, func(context.Context, EntryDownloadTargetReader) error) error) *EntryDownloadTargetService {
	// 1. 检查并保存快照入口。
	if withSnapshot == nil {
		panic("下载目标服务需要只读快照方法")
	}
	return &EntryDownloadTargetService{withSnapshot: withSnapshot}
}

// Get 为已认证用户检查下载目标，并在快照成功结束后返回独立结果。
// 目标在完整路径可见且版本匹配时才读取内容引用。
func (targetService *EntryDownloadTargetService) Get(ctx context.Context, actor domain.Actor, entryID, expectedRevision uint64) (*domain.EntryDownloadTarget, error) {
	// 1. 检查身份、目录项 ID 和预期版本。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || entryID == 0 || expectedRevision == 0 {
		return nil, fmt.Errorf("读取下载目标需要上下文、完整认证身份、非零目录项 ID 和预期版本")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 在同一快照内检查完整路径、文件类型和版本。
	var prepared *domain.EntryDownloadTarget
	snapshotError := targetService.withSnapshot(ctx, func(snapshotContext context.Context, reader EntryDownloadTargetReader) error {
		if snapshotContext == nil || reader == nil {
			return fmt.Errorf("读取下载目标需要快照上下文和读取能力")
		}
		path, err := readFileEntryPath(snapshotContext, reader, actor.UserID, entryID)
		if err != nil {
			return err
		}
		entry := path.Entries[len(path.Entries)-1]
		if entry.EntryType != "FILE" {
			return domain.ErrFileEntryNotFile
		}
		if err := ValidateEntryRevision(entry.Revision, expectedRevision); err != nil {
			return err
		}

		// 3. 读取并核对内容引用，准备下载目标。
		content, queryError := reader.FindUserFileEntryContent(snapshotContext, actor.UserID, entryID)
		if err := snapshotContext.Err(); err != nil {
			return err
		}
		if queryError != nil {
			return fmt.Errorf("读取下载目标的内容引用失败: %w", queryError)
		}
		if content == nil || content.ID != entryID || content.UserID != actor.UserID {
			return domain.ErrInvalidFileEntryData
		}
		if content.ObjectID == nil {
			return domain.ErrDownloadTargetUnavailable
		}
		if *content.ObjectID == 0 {
			return domain.ErrInvalidFileEntryData
		}
		prepared = &domain.EntryDownloadTarget{
			EntryID:   entry.ID,
			UserID:    entry.UserID,
			Name:      entry.Name,
			Revision:  entry.Revision,
			ObjectID:  *content.ObjectID,
			SizeBytes: entry.FileSize,
			MIMEType:  copyOptionalValue(content.MIMEType),
		}
		return nil
	})

	// 4. 确认快照成功结束后交付结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshotError != nil {
		return nil, fmt.Errorf("读取下载目标快照失败: %w", snapshotError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
