package service

import (
	"bytes"
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// NameEntryListReader 汇集同一快照中的名称候选和批量可见性读取。
type NameEntryListReader interface {
	FileEntryVisibilityReader
	// ListUserNameEntryCandidates 读取当前用户按名称键及 ID 升序排列的候选。
	ListUserNameEntryCandidates(context.Context, uint64, domain.GlobalEntryQuery, *domain.NameEntryPosition) (*domain.NameEntryPage, error)
}

// NameEntryListService 组合名称候选扫描与完整祖先链可见性检查。
type NameEntryListService struct {
	// withSnapshot 同步执行一次回调，成功确认后返回 nil。
	withSnapshot func(context.Context, func(context.Context, NameEntryListReader) error) error
}

// NewNameEntryListService 绑定已有只读快照入口，缺少依赖属于装配错误。
func NewNameEntryListService(withSnapshot func(context.Context, func(context.Context, NameEntryListReader) error) error) *NameEntryListService {
	// 1. 检查并保存同一快照内的读取回调。
	if withSnapshot == nil {
		panic("名称列表服务需要只读快照方法")
	}
	return &NameEntryListService{withSnapshot: withSnapshot}
}

// List 读取 all/starred、name_asc 的可见目录项，支持 Search 和 Kind。
// 每批至多一百项，单次至多检查一千个候选，页满时保留批次余项供下次重新读取。
// 失败或取消返回 nil，续页位置来自最后消费的候选并允许空页继续翻页。
func (listService *NameEntryListService) List(ctx context.Context, actor domain.Actor, query domain.GlobalEntryQuery, after *domain.NameEntryPosition) (*domain.NameEntryList, error) {
	// 1. 校验身份、查询及名称位置，复制输入名称键。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !query.IsValid() ||
		(query.Scope != domain.GlobalEntryScopeAll && query.Scope != domain.GlobalEntryScopeStarred) || query.Sort != domain.GlobalEntrySortNameAsc {
		return nil, fmt.Errorf("名称列表需要上下文、完整身份及 all/starred、name_asc 的合法查询")
	}
	if after != nil && !after.IsValid() {
		return nil, fmt.Errorf("名称翻页位置需要非零 ID 和 1～768 字节名称键")
	}
	lower := copyNameEntryPosition(after)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 进入同一只读快照，准备结果及扫描预算。
	var prepared *domain.NameEntryList
	snapshotError := listService.withSnapshot(ctx, func(snapshotContext context.Context, reader NameEntryListReader) error {
		if snapshotContext == nil || reader == nil {
			return fmt.Errorf("名称列表回调需要快照上下文和读取能力")
		}
		result := &domain.NameEntryList{Entries: make([]domain.NameEntryCandidate, 0, query.PageSize)}
		remaining := domain.MaxGlobalEntryCandidates
		seenIDs := make(map[uint64]bool)
		for remaining > 0 {
			// 3. 分批读取候选，核对严格升序并复制记录。
			if err := snapshotContext.Err(); err != nil {
				return err
			}
			batchQuery := query
			batchQuery.PageSize = min(100, remaining)
			page, readError := reader.ListUserNameEntryCandidates(snapshotContext, actor.UserID, batchQuery, copyNameEntryPosition(lower))
			if err := snapshotContext.Err(); err != nil {
				return err
			}
			if readError != nil {
				return fmt.Errorf("读取名称候选批次失败: %w", readError)
			}
			if page == nil || len(page.Entries) > batchQuery.PageSize || (page.HasMore && len(page.Entries) != batchQuery.PageSize) {
				return domain.ErrInvalidFileEntryData
			}
			candidates := make([]domain.NameEntryCandidate, len(page.Entries))
			entries := make([]domain.FileEntry, len(page.Entries))
			hasMore := page.HasMore
			previous := copyNameEntryPosition(lower)
			for index, candidate := range page.Entries {
				entry := candidate.Entry
				position := domain.NameEntryPosition{NameKey: candidate.NameKey, ID: entry.ID}
				modifiedPosition := domain.FileEntryPosition{ModifiedAt: entry.ModifiedAt, ID: entry.ID}
				if !position.IsValid() || !modifiedPosition.IsValid() || seenIDs[entry.ID] || entry.UserID != actor.UserID ||
					entry.Status != "ACTIVE" || entry.Revision == 0 ||
					entry.ParentID == nil || *entry.ParentID == 0 || *entry.ParentID == entry.ID || entry.Name == "" || !utf8.ValidString(entry.Name) ||
					(entry.EntryType != "FILE" && entry.EntryType != "DIR") || (entry.EntryType == "DIR" && entry.FileSize != 0) {
					return domain.ErrInvalidFileEntryData
				}
				if previous != nil {
					comparison := bytes.Compare(position.NameKey, previous.NameKey)
					if comparison < 0 || (comparison == 0 && position.ID <= previous.ID) {
						return domain.ErrInvalidFileEntryData
					}
				}
				seenIDs[entry.ID] = true
				candidate.NameKey = bytes.Clone(candidate.NameKey)
				previous = &domain.NameEntryPosition{NameKey: candidate.NameKey, ID: entry.ID}
				candidate.Entry.ParentID = copyOptionalValue(entry.ParentID)
				candidates[index], entries[index] = candidate, candidate.Entry
			}

			// 4. 检查完整祖先链，按原顺序收集可见项。
			visible, err := FilterVisibleFileEntries(snapshotContext, actor, entries, reader)
			if err != nil {
				return err
			}
			remaining -= len(candidates)
			visibleIndex := 0
			for index, candidate := range candidates {
				position := domain.NameEntryPosition{NameKey: candidate.NameKey, ID: candidate.Entry.ID}
				lower = &position
				if visibleIndex < len(visible) && visible[visibleIndex].ID == candidate.Entry.ID {
					result.Entries = append(result.Entries, domain.NameEntryCandidate{Entry: visible[visibleIndex], NameKey: candidate.NameKey})
					visibleIndex++
				}
				// 5. 根据页满、候选耗尽或预算耗尽保存续页位置。
				if len(result.Entries) == query.PageSize {
					result.HasMore = index+1 < len(candidates) || hasMore
					if result.HasMore {
						result.NextPosition = copyNameEntryPosition(lower)
					}
					prepared = result
					return nil
				}
			}
			if !hasMore {
				prepared = result
				return nil
			}
		}
		result.HasMore, result.NextPosition = true, copyNameEntryPosition(lower)
		prepared = result
		return nil
	})

	// 6. 确认快照成功结束后交付结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshotError != nil {
		return nil, fmt.Errorf("读取名称列表快照失败: %w", snapshotError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}

// copyNameEntryPosition 复制可空名称位置及其字节键，隔离输入、读取依赖和结果。
func copyNameEntryPosition(source *domain.NameEntryPosition) *domain.NameEntryPosition {
	// 1. 复制可空位置值，再复制底层名称键字节。
	position := copyOptionalValue(source)
	if position != nil {
		position.NameKey = bytes.Clone(position.NameKey)
	}
	return position
}
