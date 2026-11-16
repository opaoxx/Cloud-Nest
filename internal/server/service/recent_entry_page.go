package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RecentEntryPageService 组合最近使用游标校验、已确认列表读取及下一页签名。
type RecentEntryPageService struct {
	// verifyCursor 绑定 RecentEntryCursorVerifier.Verify。
	verifyCursor func(uint64, domain.GlobalEntryQuery, string) (*domain.RecentEntryPosition, error)
	// listEntries 绑定 RecentEntryListService.List，其结果已完成同快照读取。
	listEntries func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.RecentEntryPosition) (*domain.RecentEntryList, error)
	// signCursor 绑定 RecentEntryCursorSigner.Sign，接收最后消费的候选访问位置。
	signCursor func(uint64, domain.GlobalEntryQuery, domain.RecentEntryPosition) (string, error)
}

// NewRecentEntryPageService 保存三个已有能力，缺少依赖属于装配错误。
func NewRecentEntryPageService(
	verifyCursor func(uint64, domain.GlobalEntryQuery, string) (*domain.RecentEntryPosition, error),
	listEntries func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.RecentEntryPosition) (*domain.RecentEntryList, error),
	signCursor func(uint64, domain.GlobalEntryQuery, domain.RecentEntryPosition) (string, error),
) *RecentEntryPageService {
	// 1. 检查并保存游标及列表方法。
	if verifyCursor == nil || listEntries == nil || signCursor == nil {
		panic("最近使用分页服务需要游标校验、列表和签名方法")
	}
	return &RecentEntryPageService{verifyCursor: verifyCursor, listEntries: listEntries, signCursor: signCursor}
}

// List 处理 recent/last_opened_desc 的首页及文本游标续页，允许预算空页继续翻页。
// 保留文件的访问事实，取消或任何失败均返回 nil。
func (pageService *RecentEntryPageService) List(ctx context.Context, actor domain.Actor, query domain.GlobalEntryQuery, cursor string) (*domain.RecentEntryPageResult, error) {
	// 1. 校验身份、最近使用查询、游标长度和上下文。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !query.IsValid() ||
		query.Scope != domain.GlobalEntryScopeRecent || query.Sort != domain.GlobalEntrySortLastOpenedDesc {
		return nil, fmt.Errorf("最近使用分页需要上下文、完整身份及 recent/last_opened_desc 的合法查询")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(cursor) > domain.MaxRecentEntryCursorLength {
		return nil, domain.ErrInvalidRecentEntryCursor
	}

	// 2. 校验非空游标，取得独立的访问起始位置。
	var before *domain.RecentEntryPosition
	if cursor != "" {
		position, err := pageService.verifyCursor(actor.UserID, query, cursor)
		if cancellation := ctx.Err(); cancellation != nil {
			return nil, cancellation
		}
		if err != nil {
			return nil, fmt.Errorf("校验最近使用分页游标失败: %w", err)
		}
		if position == nil || !position.IsValid() {
			return nil, domain.ErrInvalidFileEntryData
		}
		before = copyOptionalValue(position)
		before.LastOpenedAt = before.LastOpenedAt.UTC()
	}

	// 3. 读取已确认的可见列表，核对并复制文件和访问事实。
	listed, err := pageService.listEntries(ctx, actor, query, copyOptionalValue(before))
	if cancellation := ctx.Err(); cancellation != nil {
		return nil, cancellation
	}
	if err != nil {
		return nil, fmt.Errorf("读取最近使用分页结果失败: %w", err)
	}
	if listed == nil || len(listed.Entries) > query.PageSize || listed.HasMore != (listed.NextPosition != nil) ||
		(query.Kind == "folder" && len(listed.Entries) != 0) {
		return nil, domain.ErrInvalidFileEntryData
	}
	next := copyOptionalValue(listed.NextPosition)
	result := &domain.RecentEntryPageResult{Entries: make([]domain.RecentEntryCandidate, 0, len(listed.Entries))}
	previous := copyOptionalValue(before)
	seenIDs := make(map[uint64]bool, len(listed.Entries))
	for _, candidate := range listed.Entries {
		entry := candidate.Entry
		position := domain.RecentEntryPosition{LastOpenedAt: candidate.LastOpenedAt, ID: entry.ID}
		modifiedPosition := domain.FileEntryPosition{ModifiedAt: entry.ModifiedAt, ID: entry.ID}
		if !position.IsValid() || !modifiedPosition.IsValid() || seenIDs[entry.ID] || entry.UserID != actor.UserID ||
			entry.Status != "ACTIVE" || entry.EntryType != "FILE" || entry.Revision == 0 ||
			entry.ParentID == nil || *entry.ParentID == 0 || entry.Name == "" {
			return nil, domain.ErrInvalidFileEntryData
		}
		if previous != nil && (position.LastOpenedAt.After(previous.LastOpenedAt) || (position.LastOpenedAt.Equal(previous.LastOpenedAt) && position.ID >= previous.ID)) {
			return nil, domain.ErrInvalidFileEntryData
		}
		seenIDs[entry.ID] = true
		previous = &position
		candidate.Entry.ParentID = copyOptionalValue(entry.ParentID)
		result.Entries = append(result.Entries, candidate)
	}

	// 4. 检查续页位置，签发下一页游标并交付。
	if next != nil {
		if !next.IsValid() {
			return nil, domain.ErrInvalidFileEntryData
		}
		next.LastOpenedAt = next.LastOpenedAt.UTC()
		if before != nil && (next.LastOpenedAt.After(before.LastOpenedAt) || (next.LastOpenedAt.Equal(before.LastOpenedAt) && next.ID >= before.ID)) {
			return nil, domain.ErrInvalidFileEntryData
		}
		if previous != nil && (next.LastOpenedAt.After(previous.LastOpenedAt) || (next.LastOpenedAt.Equal(previous.LastOpenedAt) && next.ID > previous.ID)) {
			return nil, domain.ErrInvalidFileEntryData
		}
		nextCursor, err := pageService.signCursor(actor.UserID, query, *next)
		if cancellation := ctx.Err(); cancellation != nil {
			return nil, cancellation
		}
		if err != nil {
			return nil, fmt.Errorf("签发最近使用下一页游标失败: %w", err)
		}
		if nextCursor == "" || len(nextCursor) > domain.MaxRecentEntryCursorLength {
			return nil, fmt.Errorf("最近使用下一页游标签名结果异常")
		}
		result.NextCursor = &nextCursor
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
