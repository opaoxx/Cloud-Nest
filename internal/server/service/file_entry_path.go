package service

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// FileEntryPathReader 列出路径检查在同一快照内使用的只读能力。
type FileEntryPathReader interface {
	// FindUserProfile 提供当前账号状态和真实活动根目录。
	FindUserProfile(ctx context.Context, userID uint64) (*domain.UserProfile, error)
	// FindUserFileEntry 在指定用户范围内读取文件或目录基础记录。
	FindUserFileEntry(ctx context.Context, userID, entryID uint64) (*domain.FileEntry, error)
}

// FileEntryPathService 在只读快照内组合目录项与全部祖先的可见性检查。
type FileEntryPathService struct {
	// withSnapshot 同步执行一次读取回调，成功结束后返回 nil。
	withSnapshot func(context.Context, func(context.Context, FileEntryPathReader) error) error
}

// NewFileEntryPathService 绑定已有快照入口，缺少依赖属于装配错误。
func NewFileEntryPathService(withSnapshot func(context.Context, func(context.Context, FileEntryPathReader) error) error) *FileEntryPathService {
	// 1. 检查并保存同一快照内的读取能力。
	if withSnapshot == nil {
		panic("目录项路径服务需要只读快照方法")
	}
	return &FileEntryPathService{withSnapshot: withSnapshot}
}

// Get 为已认证 Actor 读取目标路径，成功结果包含根目录和目标项。
// 目标或祖先不可见返回 ErrFileEntryNotFound，异常父链返回 ErrInvalidFileEntryData。
// 根目录深度为 0，目标最多 32 层；快照失败时返回空结果。
func (pathService *FileEntryPathService) Get(ctx context.Context, actor domain.Actor, entryID uint64) (*domain.FileEntryPath, error) {
	// 1. 检查认证身份、目标 ID 和请求上下文。
	if ctx == nil || actor.UserID == 0 || actor.SessionID == 0 || actor.DeviceID == 0 || entryID == 0 {
		return nil, fmt.Errorf("读取目录项路径需要上下文、完整认证身份和非零目标 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 进入只读快照，读取当前用户的有效资料与真实根目录。
	var prepared *domain.FileEntryPath
	snapshotError := pathService.withSnapshot(ctx, func(snapshotContext context.Context, reader FileEntryPathReader) error {
		if snapshotContext == nil || reader == nil {
			return fmt.Errorf("目录项路径回调需要快照上下文和读取能力")
		}
		profile, err := NewUserProfileService(reader.FindUserProfile).Get(snapshotContext, actor.UserID)
		if err != nil {
			return err
		}

		// 3. 从目标沿父目录向上读取，校验状态、类型、循环和深度。
		entries := make([]domain.FileEntry, 0)
		visited := make(map[uint64]bool)
		currentID := entryID
		for {
			if err := snapshotContext.Err(); err != nil {
				return err
			}
			if len(entries) > domain.MaxFileEntryDepth || visited[currentID] {
				return domain.ErrInvalidFileEntryData
			}
			visited[currentID] = true
			entry, queryError := reader.FindUserFileEntry(snapshotContext, actor.UserID, currentID)
			if err := snapshotContext.Err(); err != nil {
				return err
			}
			if errors.Is(queryError, domain.ErrFileEntryNotFound) && len(entries) > 0 {
				return domain.ErrInvalidFileEntryData
			}
			if queryError != nil {
				return fmt.Errorf("读取目录项路径失败: %w", queryError)
			}
			if entry == nil || entry.ID != currentID || entry.UserID != actor.UserID || entry.Revision == 0 {
				return domain.ErrInvalidFileEntryData
			}
			switch entry.Status {
			case "ACTIVE":
			case "TRASHED", "PURGING", "PURGED":
				return domain.ErrFileEntryNotFound
			default:
				return domain.ErrInvalidFileEntryData
			}
			if entry.EntryType != "FILE" && entry.EntryType != "DIR" {
				return domain.ErrInvalidFileEntryData
			}
			if len(entries) > 0 && entry.EntryType != "DIR" {
				return domain.ErrInvalidFileEntryData
			}
			if entry.EntryType == "DIR" && entry.FileSize != 0 {
				return domain.ErrInvalidFileEntryData
			}

			// 每次读取立即复制父目录指针，保留本次路径自己的值。
			entryCopy := *entry
			if entry.ParentID != nil {
				parentID := *entry.ParentID
				entryCopy.ParentID = &parentID
			}
			entries = append(entries, entryCopy)
			if entry.ParentID == nil {
				if entry.ID != profile.RootEntryID || entry.EntryType != "DIR" || entry.Name != "" {
					return domain.ErrInvalidFileEntryData
				}
				break
			}
			if entry.ID == profile.RootEntryID || entry.Name == "" || *entry.ParentID == 0 {
				return domain.ErrInvalidFileEntryData
			}
			currentID = *entryCopy.ParentID
		}

		// 4. 复制并整理根目录到目标的路径，快照成功结束后返回。
		slices.Reverse(entries)
		prepared = &domain.FileEntryPath{Entries: entries}
		return nil
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshotError != nil {
		return nil, fmt.Errorf("读取目录项路径快照失败: %w", snapshotError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
