package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// RecentEntryListReader 汇集同一快照中的最近候选和批量可见性读取。
type RecentEntryListReader interface {
	FileEntryVisibilityReader
	// ListUserRecentEntryCandidates 读取当前用户按访问时间及 ID 降序排列的候选。
	ListUserRecentEntryCandidates(context.Context, uint64, domain.GlobalEntryQuery, *domain.RecentEntryPosition) (*domain.RecentEntryPage, error)
}

// RecentEntryListService 组合最近候选扫描与完整祖先链可见性检查。
type RecentEntryListService struct {
	// withSnapshot 同步执行一次回调，成功确认后返回 nil。
	withSnapshot func(context.Context, func(context.Context, RecentEntryListReader) error) error
}

// NewRecentEntryListService 绑定已有只读快照入口，缺少依赖属于装配错误。
func NewRecentEntryListService(withSnapshot func(context.Context, func(context.Context, RecentEntryListReader) error) error) *RecentEntryListService {
	// 1. 检查并保存同一快照内的读取回调。
	if withSnapshot == nil {
		panic("最近列表服务需要只读快照方法")
	}
	return &RecentEntryListService{withSnapshot: withSnapshot}
}

// List 读取 recent/last_opened_desc 的可见文件，支持 Search 和 Kind。
// 每批至多一百项，单次至多检查一千个候选，页满时保留批次余项供下次重新读取。
// 失败或取消返回 nil，续页位置来自最后消费的候选并允许空页继续翻页。
func (listService *RecentEntryListService) List(ctx context.Context, actor domain.Actor, query domain.GlobalEntryQuery, before *domain.RecentEntryPosition) (*domain.RecentEntryList, error) {
	// 1. 校验身份、最近使用查询和访问位置。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !query.IsValid() ||
		query.Scope != domain.GlobalEntryScopeRecent || query.Sort != domain.GlobalEntrySortLastOpenedDesc {
		return nil, fmt.Errorf("最近列表需要上下文、完整身份及 recent/last_opened_desc 的合法查询")
	}
	upper := copyOptionalValue(before)
	if upper != nil {
		if !upper.IsValid() {
			return nil, fmt.Errorf("最近翻页位置需要非零 ID 和数据库微秒时间")
		}
		upper.LastOpenedAt = upper.LastOpenedAt.UTC()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 进入同一只读快照，准备结果及扫描预算。
	var prepared *domain.RecentEntryList
	snapshotError := listService.withSnapshot(ctx, func(snapshotContext context.Context, reader RecentEntryListReader) error {
		if snapshotContext == nil || reader == nil {
			return fmt.Errorf("最近列表回调需要快照上下文和读取能力")
		}
		result := &domain.RecentEntryList{Entries: make([]domain.RecentEntryCandidate, 0, query.PageSize)}
		remaining := domain.MaxGlobalEntryCandidates
		seenIDs := make(map[uint64]bool)
		for remaining > 0 {
			// 3. 分批读取最近候选，核对访问顺序并复制记录。
			if err := snapshotContext.Err(); err != nil {
				return err
			}
			batchQuery := query
			batchQuery.PageSize = min(100, remaining)
			page, readError := reader.ListUserRecentEntryCandidates(snapshotContext, actor.UserID, batchQuery, copyOptionalValue(upper))
			if err := snapshotContext.Err(); err != nil {
				return err
			}
			if readError != nil {
				return fmt.Errorf("读取最近候选批次失败: %w", readError)
			}
			if page == nil || len(page.Entries) > batchQuery.PageSize || (page.HasMore && len(page.Entries) != batchQuery.PageSize) ||
				(query.Kind == "folder" && len(page.Entries) != 0) {
				return domain.ErrInvalidFileEntryData
			}
			candidates := make([]domain.RecentEntryCandidate, len(page.Entries))
			entries := make([]domain.FileEntry, len(page.Entries))
			hasMore := page.HasMore
			previous := copyOptionalValue(upper)
			for index, candidate := range page.Entries {
				entry := candidate.Entry
				position := domain.RecentEntryPosition{LastOpenedAt: candidate.LastOpenedAt, ID: entry.ID}
				modifiedPosition := domain.FileEntryPosition{ModifiedAt: entry.ModifiedAt, ID: entry.ID}
				if !position.IsValid() || !modifiedPosition.IsValid() || seenIDs[entry.ID] || entry.UserID != actor.UserID ||
					entry.Status != "ACTIVE" || entry.EntryType != "FILE" || entry.Revision == 0 ||
					entry.ParentID == nil || *entry.ParentID == 0 || entry.Name == "" {
					return domain.ErrInvalidFileEntryData
				}
				if previous != nil && (position.LastOpenedAt.After(previous.LastOpenedAt) || (position.LastOpenedAt.Equal(previous.LastOpenedAt) && position.ID >= previous.ID)) {
					return domain.ErrInvalidFileEntryData
				}
				seenIDs[entry.ID] = true
				previous = &position
				candidate.Entry.ParentID = copyOptionalValue(entry.ParentID)
				candidates[index], entries[index] = candidate, candidate.Entry
			}

			// 4. 复用完整祖先链检查，按候选顺序收集可见文件及访问时间。
			visible, err := FilterVisibleFileEntries(snapshotContext, actor, entries, reader)
			if err != nil {
				return err
			}
			remaining -= len(candidates)
			visibleIndex := 0
			for index, candidate := range candidates {
				position := domain.RecentEntryPosition{LastOpenedAt: candidate.LastOpenedAt.UTC(), ID: candidate.Entry.ID}
				upper = &position
				if visibleIndex < len(visible) && visible[visibleIndex].ID == candidate.Entry.ID {
					result.Entries = append(result.Entries, domain.RecentEntryCandidate{Entry: visible[visibleIndex], LastOpenedAt: candidate.LastOpenedAt})
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
		return nil, fmt.Errorf("读取最近列表快照失败: %w", snapshotError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
