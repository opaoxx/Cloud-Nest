package service

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SizeEntryPageService 组合大小游标校验、已确认列表读取及下一页签名。
type SizeEntryPageService struct {
	// verifyCursor 绑定 SizeEntryCursorVerifier.Verify。
	verifyCursor func(uint64, domain.GlobalEntryQuery, string) (*domain.SizeEntryPosition, error)
	// listEntries 绑定 SizeEntryListService.List，其结果已完成同快照读取。
	listEntries func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.SizeEntryPosition) (*domain.SizeEntryList, error)
	// signCursor 绑定 SizeEntryCursorSigner.Sign，接收最后消费的候选位置。
	signCursor func(uint64, domain.GlobalEntryQuery, domain.SizeEntryPosition) (string, error)
}

// NewSizeEntryPageService 保存三个已有能力，缺少依赖属于装配错误。
func NewSizeEntryPageService(
	verifyCursor func(uint64, domain.GlobalEntryQuery, string) (*domain.SizeEntryPosition, error),
	listEntries func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.SizeEntryPosition) (*domain.SizeEntryList, error),
	signCursor func(uint64, domain.GlobalEntryQuery, domain.SizeEntryPosition) (string, error),
) *SizeEntryPageService {
	// 1. 检查并保存游标及列表方法。
	if verifyCursor == nil || listEntries == nil || signCursor == nil {
		panic("大小分页服务需要游标校验、列表和签名方法")
	}
	return &SizeEntryPageService{verifyCursor: verifyCursor, listEntries: listEntries, signCursor: signCursor}
}

// List 处理 all/starred 大小降序查询的首页及文本游标续页，允许预算空页继续翻页。
// 用户和查询沿用已有绑定，取消或任何失败均返回 nil。
func (pageService *SizeEntryPageService) List(ctx context.Context, actor domain.Actor, query domain.GlobalEntryQuery, cursor string) (*domain.SizeEntryPageResult, error) {
	// 1. 校验身份、查询、游标长度和上下文。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !query.IsValid() ||
		(query.Scope != domain.GlobalEntryScopeAll && query.Scope != domain.GlobalEntryScopeStarred) || query.Sort != domain.GlobalEntrySortSizeDesc {
		return nil, fmt.Errorf("大小分页需要上下文、完整身份及 all/starred、size_desc 的合法查询")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(cursor) > domain.MaxSizeEntryCursorLength {
		return nil, domain.ErrInvalidSizeEntryCursor
	}

	// 2. 校验非空游标，复制大小起始位置。
	var before *domain.SizeEntryPosition
	if cursor != "" {
		position, err := pageService.verifyCursor(actor.UserID, query, cursor)
		if cancellation := ctx.Err(); cancellation != nil {
			return nil, cancellation
		}
		if err != nil {
			return nil, fmt.Errorf("校验大小分页游标失败: %w", err)
		}
		if position == nil || !position.IsValid() {
			return nil, domain.ErrInvalidFileEntryData
		}
		before = copyOptionalValue(position)
	}

	// 3. 读取可见列表，检查大小降序并复制结果。
	listed, err := pageService.listEntries(ctx, actor, query, copyOptionalValue(before))
	if cancellation := ctx.Err(); cancellation != nil {
		return nil, cancellation
	}
	if err != nil {
		return nil, fmt.Errorf("读取大小分页结果失败: %w", err)
	}
	if listed == nil || len(listed.Entries) > query.PageSize || listed.HasMore != (listed.NextPosition != nil) {
		return nil, domain.ErrInvalidFileEntryData
	}
	next := copyOptionalValue(listed.NextPosition)
	result := &domain.SizeEntryPageResult{Entries: copyFileEntryRecords(listed.Entries)}
	previous := copyOptionalValue(before)
	seenIDs := make(map[uint64]bool, len(result.Entries))
	for _, entry := range result.Entries {
		position := domain.SizeEntryPosition{FileSize: entry.FileSize, ID: entry.ID}
		modifiedPosition := domain.FileEntryPosition{ModifiedAt: entry.ModifiedAt, ID: entry.ID}
		if !position.IsValid() || !modifiedPosition.IsValid() || seenIDs[entry.ID] || entry.UserID != actor.UserID || entry.Status != "ACTIVE" || entry.Revision == 0 ||
			entry.ParentID == nil || *entry.ParentID == 0 || *entry.ParentID == entry.ID || entry.Name == "" || !utf8.ValidString(entry.Name) ||
			(entry.EntryType != "FILE" && entry.EntryType != "DIR") || (entry.EntryType == "DIR" && entry.FileSize != 0) {
			return nil, domain.ErrInvalidFileEntryData
		}
		if previous != nil && (position.FileSize > previous.FileSize || (position.FileSize == previous.FileSize && position.ID >= previous.ID)) {
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
		if before != nil && (next.FileSize > before.FileSize || (next.FileSize == before.FileSize && next.ID >= before.ID)) {
			return nil, domain.ErrInvalidFileEntryData
		}
		if previous != nil && (next.FileSize > previous.FileSize || (next.FileSize == previous.FileSize && next.ID > previous.ID)) {
			return nil, domain.ErrInvalidFileEntryData
		}
		nextCursor, err := pageService.signCursor(actor.UserID, query, *next)
		if cancellation := ctx.Err(); cancellation != nil {
			return nil, cancellation
		}
		if err != nil {
			return nil, fmt.Errorf("签发大小下一页游标失败: %w", err)
		}
		if nextCursor == "" || len(nextCursor) > domain.MaxSizeEntryCursorLength {
			return nil, fmt.Errorf("大小下一页游标签名结果异常")
		}
		result.NextCursor = &nextCursor
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
