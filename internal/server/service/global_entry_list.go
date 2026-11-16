package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// GlobalEntryListReader 汇集同一快照中的候选读取与批量可见性读取。
type GlobalEntryListReader interface {
	FileEntryVisibilityReader
	// ListUserGlobalEntryCandidates 读取当前用户符合查询条件的有序候选。
	ListUserGlobalEntryCandidates(context.Context, uint64, domain.GlobalEntryQuery, *domain.FileEntryPosition) (*domain.FileEntryPage, error)
}

// GlobalEntryListService 组合全局候选分批扫描和完整路径可见性检查。
type GlobalEntryListService struct {
	// withSnapshot 同步执行一次回调，成功确认后返回 nil。
	withSnapshot func(context.Context, func(context.Context, GlobalEntryListReader) error) error
}

// NewGlobalEntryListService 绑定已有快照入口，缺少依赖属于装配错误。
func NewGlobalEntryListService(withSnapshot func(context.Context, func(context.Context, GlobalEntryListReader) error) error) *GlobalEntryListService {
	// 1. 检查并保存同一快照内的读取回调。
	if withSnapshot == nil {
		panic("全局列表服务需要只读快照方法")
	}
	return &GlobalEntryListService{withSnapshot: withSnapshot}
}

// List 读取 all/modified_desc 的可见列表，支持已有 Search 和 Kind 条件。
// 每批至多一百项，单次至多检查一千个候选，页满时保留批次余项供下次重新读取。
// 失败或取消返回 nil，续页位置来自最后消费的候选并允许空页继续翻页。
func (listService *GlobalEntryListService) List(ctx context.Context, actor domain.Actor, query domain.GlobalEntryQuery, before *domain.FileEntryPosition) (*domain.GlobalEntryList, error) {
	// 1. 校验身份、查询条件和翻页位置。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !query.IsValid() ||
		query.Scope != domain.GlobalEntryScopeAll || query.Sort != domain.GlobalEntrySortModifiedDesc {
		return nil, fmt.Errorf("全局列表需要上下文、完整身份及 all/modified_desc 的合法查询")
	}
	upper := copyOptionalValue(before)
	if upper != nil {
		if !upper.IsValid() {
			return nil, fmt.Errorf("全局翻页位置需要非零 ID 和数据库微秒时间")
		}
		upper.ModifiedAt = upper.ModifiedAt.UTC()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 进入同一只读快照，准备结果及扫描预算。
	var prepared *domain.GlobalEntryList
	snapshotError := listService.withSnapshot(ctx, func(snapshotContext context.Context, reader GlobalEntryListReader) error {
		if snapshotContext == nil || reader == nil {
			return fmt.Errorf("全局列表回调需要快照上下文和读取能力")
		}
		result := &domain.GlobalEntryList{Entries: make([]domain.FileEntry, 0, query.PageSize)}
		remaining := domain.MaxGlobalEntryCandidates
		seenIDs := make(map[uint64]bool)
		for remaining > 0 {
			// 3. 分批读取候选，核对范围与严格排序。
			if err := snapshotContext.Err(); err != nil {
				return err
			}
			batchQuery := query
			batchQuery.PageSize = min(100, remaining)
			page, readError := reader.ListUserGlobalEntryCandidates(snapshotContext, actor.UserID, batchQuery, copyOptionalValue(upper))
			if err := snapshotContext.Err(); err != nil {
				return err
			}
			if readError != nil {
				return fmt.Errorf("读取全局候选批次失败: %w", readError)
			}
			if page == nil || len(page.Entries) > batchQuery.PageSize || (page.HasMore && len(page.Entries) != batchQuery.PageSize) {
				return domain.ErrInvalidFileEntryData
			}
			candidates, hasMore := copyFileEntryRecords(page.Entries), page.HasMore
			previous := copyOptionalValue(upper)
			for _, entry := range candidates {
				position := domain.FileEntryPosition{ModifiedAt: entry.ModifiedAt, ID: entry.ID}
				if !position.IsValid() || seenIDs[entry.ID] || entry.UserID != actor.UserID || entry.Status != "ACTIVE" ||
					entry.Revision == 0 || entry.ParentID == nil || *entry.ParentID == 0 || entry.Name == "" {
					return domain.ErrInvalidFileEntryData
				}
				if previous != nil && (position.ModifiedAt.After(previous.ModifiedAt) || (position.ModifiedAt.Equal(previous.ModifiedAt) && position.ID >= previous.ID)) {
					return domain.ErrInvalidFileEntryData
				}
				seenIDs[entry.ID] = true
				previous = &position
			}

			// 4. 过滤隐藏路径，按候选顺序收集本页结果。
			visible, err := FilterVisibleFileEntries(snapshotContext, actor, candidates, reader)
			if err != nil {
				return err
			}
			remaining -= len(candidates)
			visibleIndex := 0
			for index, entry := range candidates {
				position := domain.FileEntryPosition{ModifiedAt: entry.ModifiedAt.UTC(), ID: entry.ID}
				upper = &position
				if visibleIndex < len(visible) && visible[visibleIndex].ID == entry.ID {
					result.Entries = append(result.Entries, visible[visibleIndex])
					visibleIndex++
				}
				// 5. 处理页满、候选耗尽及预算耗尽的下一页位置。
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
		return nil, fmt.Errorf("读取全局列表快照失败: %w", snapshotError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
