package service

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SizeEntryListReader 汇集同一快照中的候选读取与批量可见性读取。
type SizeEntryListReader interface {
	FileEntryVisibilityReader
	// ListUserSizeEntryCandidates 读取当前用户符合查询条件的有序候选。
	ListUserSizeEntryCandidates(context.Context, uint64, domain.GlobalEntryQuery, *domain.SizeEntryPosition) (*domain.FileEntryPage, error)
}

// SizeEntryListService 组合大小候选分批扫描和完整路径可见性检查。
type SizeEntryListService struct {
	// withSnapshot 同步执行一次回调，成功确认后返回 nil。
	withSnapshot func(context.Context, func(context.Context, SizeEntryListReader) error) error
}

// NewSizeEntryListService 绑定已有快照入口，缺少依赖属于装配错误。
func NewSizeEntryListService(withSnapshot func(context.Context, func(context.Context, SizeEntryListReader) error) error) *SizeEntryListService {
	// 1. 检查并保存同一快照内的读取回调。
	if withSnapshot == nil {
		panic("大小列表服务需要只读快照方法")
	}
	return &SizeEntryListService{withSnapshot: withSnapshot}
}

// List 读取 all/starred 的大小降序可见列表，支持已有 Search 和 Kind 条件。
// 每批至多一百项，单次至多检查一千个候选，页满时保留批次余项供下次重新读取。
// 失败或取消返回 nil，续页位置来自最后消费的候选并允许空页继续翻页。
func (listService *SizeEntryListService) List(ctx context.Context, actor domain.Actor, query domain.GlobalEntryQuery, before *domain.SizeEntryPosition) (*domain.SizeEntryList, error) {
	// 1. 校验身份、查询及大小位置，复制输入位置。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !query.IsValid() ||
		(query.Scope != domain.GlobalEntryScopeAll && query.Scope != domain.GlobalEntryScopeStarred) || query.Sort != domain.GlobalEntrySortSizeDesc {
		return nil, fmt.Errorf("大小列表需要上下文、完整身份及 all/starred、size_desc 的合法查询")
	}
	upper := copyOptionalValue(before)
	if upper != nil {
		if !upper.IsValid() {
			return nil, fmt.Errorf("大小翻页位置需要非零 ID")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 进入同一只读快照，准备结果及扫描预算。
	var prepared *domain.SizeEntryList
	snapshotError := listService.withSnapshot(ctx, func(snapshotContext context.Context, reader SizeEntryListReader) error {
		if snapshotContext == nil || reader == nil {
			return fmt.Errorf("大小列表回调需要快照上下文和读取能力")
		}
		result := &domain.SizeEntryList{Entries: make([]domain.FileEntry, 0, query.PageSize)}
		remaining := domain.MaxGlobalEntryCandidates
		seenIDs := make(map[uint64]bool)
		for remaining > 0 {
			// 3. 分批读取候选，核对大小和 ID 的严格降序。
			if err := snapshotContext.Err(); err != nil {
				return err
			}
			batchQuery := query
			batchQuery.PageSize = min(100, remaining)
			page, readError := reader.ListUserSizeEntryCandidates(snapshotContext, actor.UserID, batchQuery, copyOptionalValue(upper))
			if err := snapshotContext.Err(); err != nil {
				return err
			}
			if readError != nil {
				return fmt.Errorf("读取大小候选批次失败: %w", readError)
			}
			if page == nil || len(page.Entries) > batchQuery.PageSize || (page.HasMore && len(page.Entries) != batchQuery.PageSize) {
				return domain.ErrInvalidFileEntryData
			}
			candidates, hasMore := copyFileEntryRecords(page.Entries), page.HasMore
			previous := copyOptionalValue(upper)
			for _, entry := range candidates {
				position := domain.SizeEntryPosition{FileSize: entry.FileSize, ID: entry.ID}
				modifiedPosition := domain.FileEntryPosition{ModifiedAt: entry.ModifiedAt, ID: entry.ID}
				if !position.IsValid() || !modifiedPosition.IsValid() || seenIDs[entry.ID] || entry.UserID != actor.UserID || entry.Status != "ACTIVE" ||
					entry.Revision == 0 || entry.ParentID == nil || *entry.ParentID == 0 || *entry.ParentID == entry.ID || entry.Name == "" || !utf8.ValidString(entry.Name) ||
					(entry.EntryType != "FILE" && entry.EntryType != "DIR") || (entry.EntryType == "DIR" && entry.FileSize != 0) {
					return domain.ErrInvalidFileEntryData
				}
				if previous != nil && (position.FileSize > previous.FileSize || (position.FileSize == previous.FileSize && position.ID >= previous.ID)) {
					return domain.ErrInvalidFileEntryData
				}
				seenIDs[entry.ID] = true
				previous = &position
			}

			// 4. 过滤隐藏路径，按候选顺序收集可见项。
			visible, err := FilterVisibleFileEntries(snapshotContext, actor, candidates, reader)
			if err != nil {
				return err
			}
			remaining -= len(candidates)
			visibleIndex := 0
			for index, entry := range candidates {
				position := domain.SizeEntryPosition{FileSize: entry.FileSize, ID: entry.ID}
				upper = &position
				if visibleIndex < len(visible) && visible[visibleIndex].ID == entry.ID {
					result.Entries = append(result.Entries, visible[visibleIndex])
					visibleIndex++
				}
				// 5. 根据页满、候选耗尽或预算耗尽保存续页位置。
				if len(result.Entries) == query.PageSize {
					result.HasMore = index+1 < len(candidates) || hasMore
					if result.HasMore {
						result.NextPosition = copyOptionalValue(upper)
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
		result.HasMore, result.NextPosition = true, copyOptionalValue(upper)
		prepared = result
		return nil
	})

	// 6. 确认快照成功结束后交付结果。
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshotError != nil {
		return nil, fmt.Errorf("读取大小列表快照失败: %w", snapshotError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
