package client

import (
	"context"
	"errors"
	"net/url"
	"strconv"
)

// DirectoryEntry 是列表展示使用的目录项摘要。
type DirectoryEntry struct {
	ID        string `json:"id"`
	ParentID  string `json:"parent_id"`
	Name      string `json:"name"`
	EntryType string `json:"entry_type"`
	SizeBytes uint64 `json:"size_bytes"`
}

// DirectoryBreadcrumb 保存目录路径中的一个层级。
type DirectoryBreadcrumb struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// DirectoryPage 保存服务返回的目录项、路径和续页信息。
type DirectoryPage struct {
	Items       []DirectoryEntry      `json:"items"`
	Breadcrumbs []DirectoryBreadcrumb `json:"breadcrumbs"`
	NextCursor  *string               `json:"next_cursor"`
}

// ReadDirectoryPage 按真实父目录 ID 读取目录首页。
func ReadDirectoryPage(ctx context.Context, address, accessToken, parentID string) (DirectoryPage, error) {
	// 1. 校验完整的十进制 ID，并把原文放入查询参数。
	if !validEntryID(parentID) {
		return DirectoryPage{}, errors.New("服务返回的目录 ID 有误")
	}
	var result struct {
		Data DirectoryPage `json:"data"`
	}
	if err := getAuthenticatedJSON(ctx, address, accessToken, "entries", url.Values{"parent_id": {parentID}}, &result); err != nil {
		return DirectoryPage{}, err
	}

	// 2. 确认路径终点和子项归属与请求一致，空数组是可展示的空页。
	page := result.Data
	if page.Items == nil || len(page.Breadcrumbs) == 0 || len(page.Breadcrumbs) > 33 ||
		page.Breadcrumbs[len(page.Breadcrumbs)-1].ID != parentID {
		return DirectoryPage{}, errors.New("服务返回的目录页与请求不一致")
	}
	seen := make(map[string]bool, len(page.Breadcrumbs))
	for index, crumb := range page.Breadcrumbs {
		if !validEntryID(crumb.ID) || seen[crumb.ID] || (index == 0) != (crumb.Name == "") {
			return DirectoryPage{}, errors.New("服务返回的目录路径有误")
		}
		seen[crumb.ID] = true
	}
	for _, entry := range page.Items {
		if !validEntryID(entry.ID) || entry.ParentID != parentID || entry.Name == "" ||
			(entry.EntryType != "FILE" && entry.EntryType != "DIR") || entry.SizeBytes > 1<<53-1 {
			return DirectoryPage{}, errors.New("服务返回的目录项格式有误")
		}
	}
	return page, nil
}

func validEntryID(text string) bool {
	value, err := strconv.ParseUint(text, 10, 64)
	return err == nil && value != 0 && strconv.FormatUint(value, 10) == text
}
