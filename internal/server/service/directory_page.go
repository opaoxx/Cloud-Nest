package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// DirectoryPageService 组合客户端游标校验、目录列表与下一页签名。
type DirectoryPageService struct {
	// verifyCursor 绑定 DirectoryCursorVerifier.Verify。
	verifyCursor func(uint64, uint64, string) (*domain.FileEntryPosition, error)
	// listDirectory 绑定 DirectoryListService.List，成功结果已经完成快照读取。
	listDirectory func(context.Context, domain.Actor, uint64, *domain.FileEntryPosition, int) (*domain.DirectoryList, error)
	// signCursor 绑定 DirectoryCursorSigner.Sign，接收已交付页的末项位置。
	signCursor func(uint64, uint64, domain.FileEntryPosition) (string, error)
}

// NewDirectoryPageService 保存三个已有能力，缺少依赖属于装配错误。
func NewDirectoryPageService(
	verifyCursor func(uint64, uint64, string) (*domain.FileEntryPosition, error),
	listDirectory func(context.Context, domain.Actor, uint64, *domain.FileEntryPosition, int) (*domain.DirectoryList, error),
	signCursor func(uint64, uint64, domain.FileEntryPosition) (string, error),
) *DirectoryPageService {
	// 1. 检查并保存游标及目录查询方法。
	if verifyCursor == nil || listDirectory == nil || signCursor == nil {
		panic("目录分页服务需要游标校验、目录列表和游标签名方法")
	}
	return &DirectoryPageService{verifyCursor: verifyCursor, listDirectory: listDirectory, signCursor: signCursor}
}

// List 接收客户端游标，成功返回目录路径、活动子项和可空下一页游标。
// 用户身份来自认证流程，所有失败及取消均返回空结果。
func (pageService *DirectoryPageService) List(ctx context.Context, actor domain.Actor, parentID uint64, cursor string, pageSize int) (*domain.DirectoryPageResult, error) {
	// 1. 检查认证身份、目录、页大小和请求上下文。
	if ctx == nil || actor.UserID == 0 || actor.SessionID == 0 || actor.DeviceID == 0 ||
		parentID == 0 || pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("读取带游标目录页需要上下文、完整认证身份、非零目录 ID 和 1～100 的页大小")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(cursor) > domain.MaxDirectoryCursorLength {
		return nil, domain.ErrInvalidDirectoryCursor
	}

	// 2. 校验非空游标，得到内部翻页位置。
	var before *domain.FileEntryPosition
	if cursor != "" {
		position, verifyError := pageService.verifyCursor(actor.UserID, parentID, cursor)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if verifyError != nil {
			return nil, fmt.Errorf("校验目录分页游标失败: %w", verifyError)
		}
		if position == nil || !position.IsValid() {
			return nil, fmt.Errorf("目录游标校验结果异常")
		}
		positionCopy := *position
		positionCopy.ModifiedAt = positionCopy.ModifiedAt.UTC()
		before = &positionCopy
	}

	// 3. 调用目录列表服务，核对结果并复制路径和子项。
	listed, listError := pageService.listDirectory(ctx, actor, parentID, before, pageSize)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if listError != nil {
		return nil, fmt.Errorf("读取目录分页结果失败: %w", listError)
	}
	if listed == nil || len(listed.Path.Entries) == 0 || len(listed.Entries) > pageSize ||
		(listed.HasMore && len(listed.Entries) != pageSize) {
		return nil, domain.ErrInvalidFileEntryData
	}
	parent := listed.Path.Entries[len(listed.Path.Entries)-1]
	if parent.ID != parentID || parent.UserID != actor.UserID || parent.EntryType != "DIR" || parent.Status != "ACTIVE" {
		return nil, domain.ErrInvalidFileEntryData
	}
	hasMore := listed.HasMore
	result := &domain.DirectoryPageResult{
		Path:    domain.FileEntryPath{Entries: copyFileEntryRecords(listed.Path.Entries)},
		Entries: copyFileEntryRecords(listed.Entries),
	}

	// 4. 为有后续页的结果签名，返回完整分页数据。
	if hasMore {
		last := result.Entries[len(result.Entries)-1]
		position := domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID}
		if !position.IsValid() || last.UserID != actor.UserID || last.ParentID == nil || *last.ParentID != parentID {
			return nil, domain.ErrInvalidFileEntryData
		}
		nextCursor, signError := pageService.signCursor(actor.UserID, parentID, position)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if signError != nil {
			return nil, fmt.Errorf("生成目录下一页游标失败: %w", signError)
		}
		if nextCursor == "" || len(nextCursor) > domain.MaxDirectoryCursorLength {
			return nil, fmt.Errorf("目录下一页游标签名结果异常")
		}
		result.NextCursor = &nextCursor
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// copyFileEntryRecords 为路径和子项复制记录及可空父目录指针。
func copyFileEntryRecords(entries []domain.FileEntry) []domain.FileEntry {
	// 1. 分配结果，每项可空父目录独立复制。
	result := make([]domain.FileEntry, len(entries))
	for index, entry := range entries {
		result[index] = entry
		if entry.ParentID != nil {
			parentID := *entry.ParentID
			result[index].ParentID = &parentID
		}
	}
	return result
}
