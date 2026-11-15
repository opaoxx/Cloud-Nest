package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// EntryDetailReader 汇集同一快照中读取目录项详情所需的能力。
type EntryDetailReader interface {
	FileEntryPathReader
	// FindUserFileEntryDisplay 按用户和目标读取扩展展示事实。
	FindUserFileEntryDisplay(ctx context.Context, userID, entryID uint64) (*domain.FileEntryDisplay, error)
	// FindUserEntryPreference 按用户和目标读取可空偏好。
	FindUserEntryPreference(ctx context.Context, userID, entryID uint64) (*domain.EntryPreference, error)
}

// EntryDetailService 组合可见路径、扩展展示和偏好的同快照读取。
type EntryDetailService struct {
	// withSnapshot 同步执行一次回调，快照成功结束后返回 nil。
	withSnapshot func(context.Context, func(context.Context, EntryDetailReader) error) error
}

// NewEntryDetailService 绑定已有只读快照入口，缺少依赖属于装配错误。
func NewEntryDetailService(withSnapshot func(context.Context, func(context.Context, EntryDetailReader) error) error) *EntryDetailService {
	// 1. 检查并保存详情使用的快照方法。
	if withSnapshot == nil {
		panic("目录项详情服务需要只读快照方法")
	}
	return &EntryDetailService{withSnapshot: withSnapshot}
}

// Get 为已认证 Actor 读取可见文件或目录的完整内部详情。
// 各部分来自同一快照，失败或取消时返回空结果。
func (detailService *EntryDetailService) Get(ctx context.Context, actor domain.Actor, entryID uint64) (*domain.EntryDetail, error) {
	// 1. 检查认证身份、目录项 ID 和请求上下文。
	if ctx == nil || actor.UserID == 0 || actor.SessionID == 0 || actor.DeviceID == 0 || entryID == 0 {
		return nil, fmt.Errorf("读取目录项详情需要上下文、完整认证身份和非零目标 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 进入只读快照，复用路径服务检查目标及祖先可见性。
	var prepared *domain.EntryDetail
	snapshotError := detailService.withSnapshot(ctx, func(snapshotContext context.Context, reader EntryDetailReader) error {
		if snapshotContext == nil || reader == nil {
			return fmt.Errorf("目录项详情回调需要快照上下文和读取能力")
		}
		paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
			return action(ctx, reader)
		})
		path, err := paths.Get(snapshotContext, actor, entryID)
		if err != nil {
			return err
		}

		// 3. 使用同一快照读取并复制扩展展示信息。
		display, queryError := reader.FindUserFileEntryDisplay(snapshotContext, actor.UserID, entryID)
		if err := snapshotContext.Err(); err != nil {
			return err
		}
		if queryError != nil {
			return fmt.Errorf("读取详情展示信息失败: %w", queryError)
		}
		if display == nil || display.ID != entryID || display.UserID != actor.UserID ||
			(display.SourceDeviceID != nil && *display.SourceDeviceID == 0) {
			return domain.ErrInvalidFileEntryData
		}
		displayCopy := *display
		displayCopy.Category = copyOptionalValue(display.Category)
		displayCopy.Extension = copyOptionalValue(display.Extension)
		displayCopy.MIMEType = copyOptionalValue(display.MIMEType)
		displayCopy.SourceDeviceID = copyOptionalValue(display.SourceDeviceID)
		displayCopy.SourceDeviceName = copyOptionalValue(display.SourceDeviceName)
		displayCopy.ColorKey = copyOptionalValue(display.ColorKey)
		displayCopy.ContentModifiedAt = copyOptionalValue(display.ContentModifiedAt)

		// 4. 读取并复制偏好，组合完整详情结果。
		preference, queryError := reader.FindUserEntryPreference(snapshotContext, actor.UserID, entryID)
		if err := snapshotContext.Err(); err != nil {
			return err
		}
		if queryError != nil {
			return fmt.Errorf("读取详情偏好失败: %w", queryError)
		}
		if preference == nil || preference.UserID != actor.UserID || preference.EntryID != entryID {
			return domain.ErrInvalidFileEntryData
		}
		preferenceCopy := *preference
		preferenceCopy.StarredAt = copyOptionalValue(preference.StarredAt)
		preferenceCopy.LastOpenedAt = copyOptionalValue(preference.LastOpenedAt)
		prepared = &domain.EntryDetail{Path: *path, Display: displayCopy, Preference: preferenceCopy}
		return nil
	})

	// 5. 确认快照成功结束且请求有效，再返回详情。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshotError != nil {
		return nil, fmt.Errorf("读取目录项详情快照失败: %w", snapshotError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}

// copyOptionalValue 复制可空字段的值，让调用方取得独立指针。
func copyOptionalValue[Value any](source *Value) *Value {
	// 1. 保留 nil，存在值时复制后返回新地址。
	if source == nil {
		return nil
	}
	value := *source
	return &value
}
