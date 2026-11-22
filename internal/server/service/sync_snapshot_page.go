package service

import (
	"encoding/json"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncSnapshotPage 保存按固定边界交付的条目和可空的续页位置。
type SyncSnapshotPage struct {
	// Items 保持快照原有顺序，空树返回空切片。
	Items []json.RawMessage
	// NextPosition 是下一页的零基起点，末页为 nil。
	NextPosition *int
}

// Page 从本次已验证的快照读取一页。第一页从位置零开始。
func (snapshot *CachedSyncSnapshot) Page(position int) (*SyncSnapshotPage, error) {
	// 1. 核对固定页边界。空树只允许读取位置零。
	if snapshot == nil || snapshot.pageSize <= 0 || position < 0 || position%snapshot.pageSize != 0 ||
		(position > 0 && position >= snapshot.ItemCount()) {
		return nil, domain.ErrInvalidSyncSnapshotCursor
	}

	// 2. 只复制本页范围内的条目，保留缓存中的顺序与字节。
	count := snapshot.ItemCount()
	end := position + min(snapshot.pageSize, count-position)
	page := &SyncSnapshotPage{Items: make([]json.RawMessage, end-position)}
	for index := range page.Items {
		page.Items[index], _ = snapshot.Item(position + index)
	}

	// 3. 还有条目时给出下一页起点，末页结束。
	if end < count {
		page.NextPosition = &end
	}
	return page, nil
}
