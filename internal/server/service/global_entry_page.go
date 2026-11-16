package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// GlobalEntryPageService 组合全局游标校验、已确认列表读取及下一页签名。
type GlobalEntryPageService struct {
	// verifyCursor 绑定 GlobalEntryCursorVerifier.Verify。
	verifyCursor func(uint64, domain.GlobalEntryQuery, string) (*domain.FileEntryPosition, error)
	// listEntries 绑定 GlobalEntryListService.List，其结果已完成同快照读取。
	listEntries func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.FileEntryPosition) (*domain.GlobalEntryList, error)
	// signCursor 绑定 GlobalEntryCursorSigner.Sign，接收最后消费的候选位置。
	signCursor func(uint64, domain.GlobalEntryQuery, domain.FileEntryPosition) (string, error)
}

// NewGlobalEntryPageService 保存三个已有能力，缺少依赖属于装配错误。
func NewGlobalEntryPageService(
	verifyCursor func(uint64, domain.GlobalEntryQuery, string) (*domain.FileEntryPosition, error),
	listEntries func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.FileEntryPosition) (*domain.GlobalEntryList, error),
	signCursor func(uint64, domain.GlobalEntryQuery, domain.FileEntryPosition) (string, error),
) *GlobalEntryPageService {
	// 1. 检查并保存游标及列表方法。
	if verifyCursor == nil || listEntries == nil || signCursor == nil {
		panic("全局分页服务需要游标校验、列表和签名方法")
	}
	return &GlobalEntryPageService{verifyCursor: verifyCursor, listEntries: listEntries, signCursor: signCursor}
}

// List 处理 all/starred 修改时间降序查询的首页及文本游标续页，允许预算空页继续翻页。
// 用户和查询沿用已有绑定，取消或任何失败均返回 nil。
func (pageService *GlobalEntryPageService) List(ctx context.Context, actor domain.Actor, query domain.GlobalEntryQuery, cursor string) (*domain.GlobalEntryPageResult, error) {
	// 1. 校验身份、all/starred 查询、游标长度和上下文。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !query.IsValid() ||
		(query.Scope != domain.GlobalEntryScopeAll && query.Scope != domain.GlobalEntryScopeStarred) || query.Sort != domain.GlobalEntrySortModifiedDesc {
		return nil, fmt.Errorf("全局分页需要上下文、完整身份及 all/starred、modified_desc 的合法查询")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(cursor) > domain.MaxGlobalEntryCursorLength {
		return nil, domain.ErrInvalidGlobalEntryCursor
	}

	// 2. 校验非空游标并取得独立起始位置。
	var before *domain.FileEntryPosition
	if cursor != "" {
		position, err := pageService.verifyCursor(actor.UserID, query, cursor)
		if cancellation := ctx.Err(); cancellation != nil {
			return nil, cancellation
		}
		if err != nil {
			return nil, fmt.Errorf("校验全局分页游标失败: %w", err)
		}
		if position == nil || !position.IsValid() {
			return nil, domain.ErrInvalidFileEntryData
		}
		before = copyOptionalValue(position)
		before.ModifiedAt = before.ModifiedAt.UTC()
	}

	// 3. 读取已确认的可见列表，核对并复制结果。
	listed, err := pageService.listEntries(ctx, actor, query, copyOptionalValue(before))
	if cancellation := ctx.Err(); cancellation != nil {
		return nil, cancellation
	}
	if err != nil {
		return nil, fmt.Errorf("读取全局分页结果失败: %w", err)
	}
	if listed == nil || len(listed.Entries) > query.PageSize || listed.HasMore != (listed.NextPosition != nil) {
		return nil, domain.ErrInvalidFileEntryData
	}
	next := copyOptionalValue(listed.NextPosition)
	result := &domain.GlobalEntryPageResult{Entries: copyFileEntryRecords(listed.Entries)}
	previous := copyOptionalValue(before)
	seenIDs := make(map[uint64]bool, len(result.Entries))
	for _, entry := range result.Entries {
		position := domain.FileEntryPosition{ModifiedAt: entry.ModifiedAt, ID: entry.ID}
		if !position.IsValid() || seenIDs[entry.ID] || entry.UserID != actor.UserID || entry.Status != "ACTIVE" || entry.Revision == 0 ||
			entry.ParentID == nil || *entry.ParentID == 0 || entry.Name == "" ||
			(entry.EntryType != "FILE" && entry.EntryType != "DIR") || (entry.EntryType == "DIR" && entry.FileSize != 0) {
			return nil, domain.ErrInvalidFileEntryData
		}
		if previous != nil && (position.ModifiedAt.After(previous.ModifiedAt) || (position.ModifiedAt.Equal(previous.ModifiedAt) && position.ID >= previous.ID)) {
			return nil, domain.ErrInvalidFileEntryData
		}
		seenIDs[entry.ID] = true
		previous = &position
	}

	// 4. 检查续页位置，签发下一页游标并交付。
	if next != nil {
		if !next.IsValid() {
			return nil, domain.ErrInvalidFileEntryData
		}
		next.ModifiedAt = next.ModifiedAt.UTC()
		if before != nil && (next.ModifiedAt.After(before.ModifiedAt) || (next.ModifiedAt.Equal(before.ModifiedAt) && next.ID >= before.ID)) {
			return nil, domain.ErrInvalidFileEntryData
		}
		if previous != nil && (next.ModifiedAt.After(previous.ModifiedAt) || (next.ModifiedAt.Equal(previous.ModifiedAt) && next.ID > previous.ID)) {
			return nil, domain.ErrInvalidFileEntryData
		}
		nextCursor, err := pageService.signCursor(actor.UserID, query, *next)
		if cancellation := ctx.Err(); cancellation != nil {
			return nil, cancellation
		}
		if err != nil {
			return nil, fmt.Errorf("签发全局下一页游标失败: %w", err)
		}
		if nextCursor == "" || len(nextCursor) > domain.MaxGlobalEntryCursorLength {
			return nil, fmt.Errorf("全局下一页游标签名结果异常")
		}
		result.NextCursor = &nextCursor
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
