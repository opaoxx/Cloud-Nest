package domain

import "unicode/utf8"

// MaxGlobalEntryCandidates 是单次全局列表请求允许扫描的候选总数。
const MaxGlobalEntryCandidates = 1000

const (
	// GlobalEntryScopeAll 表示当前用户的全部活动普通目录项候选。
	GlobalEntryScopeAll = "all"
	// GlobalEntryScopeStarred 表示当前用户具有收藏时间的活动普通目录项候选。
	GlobalEntryScopeStarred = "starred"
	// GlobalEntryScopeRecent 表示当前用户具有访问时间的活动文件候选。
	GlobalEntryScopeRecent = "recent"
)

const (
	// GlobalEntrySortModifiedDesc 按业务修改时间、目录项 ID 降序排列。
	GlobalEntrySortModifiedDesc = "modified_desc"
	// GlobalEntrySortNameAsc 按名称排序值、目录项 ID 升序排列。
	GlobalEntrySortNameAsc = "name_asc"
	// GlobalEntrySortSizeDesc 按内容大小、目录项 ID 降序排列。
	GlobalEntrySortSizeDesc = "size_desc"
	// GlobalEntrySortLastOpenedDesc 表示最近使用的内部固定排序，按访问时间、ID 降序排列。
	GlobalEntrySortLastOpenedDesc = "last_opened_desc"
)

// GlobalEntryQuery 保存全局列表共用的业务查询条件。
// 用户范围由调用服务的 Actor 提供，候选交付前需要完成完整祖先链可见性检查。
type GlobalEntryQuery struct {
	// Scope 为 all、starred 或 recent，根目录由资料入口提供。
	Scope string
	// Sort 保存实际执行的排序，recent 使用内部固定的 last_opened_desc。
	Sort string
	// Search 保存 UTF-8 搜索文本，空字符串表示无搜索，空白及特殊字符保留为原值。
	Search string
	// Kind 为空表示全部类型，其余使用 folder 或小写文件类别。
	Kind string
	// PageSize 是已取得默认值后的交付页大小，范围为 1～100。
	PageSize int
}

// IsValid 检查范围、排序、搜索、分类及页大小是否满足全局列表约定。
func (query GlobalEntryQuery) IsValid() bool {
	// 1. 检查页大小和搜索文本编码。
	if query.PageSize < 1 || query.PageSize > 100 || !utf8.ValidString(query.Search) {
		return false
	}

	// 2. 检查全局范围与对应排序。
	switch query.Scope {
	case GlobalEntryScopeAll, GlobalEntryScopeStarred:
		if query.Sort != GlobalEntrySortModifiedDesc && query.Sort != GlobalEntrySortNameAsc && query.Sort != GlobalEntrySortSizeDesc {
			return false
		}
	case GlobalEntryScopeRecent:
		if query.Sort != GlobalEntrySortLastOpenedDesc {
			return false
		}
	default:
		return false
	}

	// 3. 检查文件夹或文件分类筛选。
	switch query.Kind {
	case "", "folder", "document", "image", "video", "audio", "archive", "other":
		return true
	default:
		return false
	}
}
