package service

import (
	"bytes"
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// NameEntryPageService 组合名称游标校验、可见列表读取和下一页签名。
type NameEntryPageService struct {
	// verifyCursor 绑定 NameEntryCursorVerifier.Verify。
	verifyCursor func(uint64, domain.GlobalEntryQuery, string) (*domain.NameEntryPosition, error)
	// listEntries 绑定 NameEntryListService.List，其结果已完成同快照读取。
	listEntries func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.NameEntryPosition) (*domain.NameEntryList, error)
	// signCursor 绑定 NameEntryCursorSigner.Sign，接收最后消费的候选名称位置。
	signCursor func(uint64, domain.GlobalEntryQuery, domain.NameEntryPosition) (string, error)
}

// NewNameEntryPageService 保存三个已有能力，缺少依赖属于装配错误。
func NewNameEntryPageService(
	verifyCursor func(uint64, domain.GlobalEntryQuery, string) (*domain.NameEntryPosition, error),
	listEntries func(context.Context, domain.Actor, domain.GlobalEntryQuery, *domain.NameEntryPosition) (*domain.NameEntryList, error),
	signCursor func(uint64, domain.GlobalEntryQuery, domain.NameEntryPosition) (string, error),
) *NameEntryPageService {
	// 1. 检查并保存游标及列表方法。
	if verifyCursor == nil || listEntries == nil || signCursor == nil {
		panic("名称分页服务需要游标校验、列表和签名方法")
	}
	return &NameEntryPageService{verifyCursor: verifyCursor, listEntries: listEntries, signCursor: signCursor}
}

// List 处理 all/starred、name_asc 的首页及文本游标续页。
// 保留原始名称键，取消或任何失败均返回 nil。
func (pageService *NameEntryPageService) List(ctx context.Context, actor domain.Actor, query domain.GlobalEntryQuery, cursor string) (*domain.NameEntryPageResult, error) {
	// 1. 校验身份、查询、游标长度和上下文。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !query.IsValid() ||
		(query.Scope != domain.GlobalEntryScopeAll && query.Scope != domain.GlobalEntryScopeStarred) || query.Sort != domain.GlobalEntrySortNameAsc {
		return nil, fmt.Errorf("名称分页需要上下文、完整身份及 all/starred、name_asc 的合法查询")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(cursor) > domain.MaxNameEntryCursorLength {
		return nil, domain.ErrInvalidNameEntryCursor
	}

	// 2. 校验非空游标，复制名称起始位置。
	var after *domain.NameEntryPosition
	if cursor != "" {
		position, err := pageService.verifyCursor(actor.UserID, query, cursor)
		if cancellation := ctx.Err(); cancellation != nil {
			return nil, cancellation
		}
		if err != nil {
			return nil, fmt.Errorf("校验名称分页游标失败: %w", err)
		}
		if position == nil || !position.IsValid() {
			return nil, domain.ErrInvalidFileEntryData
		}
		after = copyNameEntryPosition(position)
	}

	// 3. 读取可见列表，检查名称升序并复制结果。
	listed, err := pageService.listEntries(ctx, actor, query, copyNameEntryPosition(after))
	if cancellation := ctx.Err(); cancellation != nil {
		return nil, cancellation
	}
	if err != nil {
		return nil, fmt.Errorf("读取名称分页结果失败: %w", err)
	}
	if listed == nil || len(listed.Entries) > query.PageSize || listed.HasMore != (listed.NextPosition != nil) {
		return nil, domain.ErrInvalidFileEntryData
	}
	next := copyNameEntryPosition(listed.NextPosition)
	result := &domain.NameEntryPageResult{Entries: make([]domain.NameEntryCandidate, 0, len(listed.Entries))}
	previous := copyNameEntryPosition(after)
	seenIDs := make(map[uint64]bool, len(listed.Entries))
	for _, candidate := range listed.Entries {
		entry := candidate.Entry
		position := domain.NameEntryPosition{NameKey: candidate.NameKey, ID: entry.ID}
		modifiedPosition := domain.FileEntryPosition{ModifiedAt: entry.ModifiedAt, ID: entry.ID}
		if !position.IsValid() || !modifiedPosition.IsValid() || seenIDs[entry.ID] || entry.UserID != actor.UserID ||
			entry.Status != "ACTIVE" || entry.Revision == 0 || entry.ParentID == nil || *entry.ParentID == 0 ||
			*entry.ParentID == entry.ID || entry.Name == "" || !utf8.ValidString(entry.Name) ||
			(entry.EntryType != "FILE" && entry.EntryType != "DIR") || (entry.EntryType == "DIR" && entry.FileSize != 0) {
			return nil, domain.ErrInvalidFileEntryData
		}
		if previous != nil {
			comparison := bytes.Compare(position.NameKey, previous.NameKey)
			if comparison < 0 || (comparison == 0 && position.ID <= previous.ID) {
				return nil, domain.ErrInvalidFileEntryData
			}
		}
		seenIDs[entry.ID] = true
		candidate.NameKey = bytes.Clone(candidate.NameKey)
		candidate.Entry.ParentID = copyOptionalValue(entry.ParentID)
		previous = &domain.NameEntryPosition{NameKey: candidate.NameKey, ID: entry.ID}
		result.Entries = append(result.Entries, candidate)
	}

	// 4. 检查续页位置，签发下一页游标并交付。
	if next != nil {
		if !next.IsValid() {
			return nil, domain.ErrInvalidFileEntryData
		}
		if after != nil {
			comparison := bytes.Compare(next.NameKey, after.NameKey)
			if comparison < 0 || (comparison == 0 && next.ID <= after.ID) {
				return nil, domain.ErrInvalidFileEntryData
			}
		}
		if previous != nil {
			comparison := bytes.Compare(next.NameKey, previous.NameKey)
			if comparison < 0 || (comparison == 0 && next.ID < previous.ID) {
				return nil, domain.ErrInvalidFileEntryData
			}
		}
		nextCursor, err := pageService.signCursor(actor.UserID, query, *next)
		if cancellation := ctx.Err(); cancellation != nil {
			return nil, cancellation
		}
		if err != nil {
			return nil, fmt.Errorf("签发名称下一页游标失败: %w", err)
		}
		if nextCursor == "" || len(nextCursor) > domain.MaxNameEntryCursorLength {
			return nil, fmt.Errorf("名称下一页游标签名结果异常")
		}
		result.NextCursor = &nextCursor
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
