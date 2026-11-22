package dto

import (
	"encoding/json"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncSnapshotEntriesJSON 保存可按位置分页的条目 JSON 和整份数组的字节数。
type SyncSnapshotEntriesJSON struct {
	// Items 保持输入条目顺序，空树编码为 []。
	Items []json.RawMessage
	// SizeBytes 包含条目、方括号和逗号，可用于整份快照的预算检查。
	SizeBytes int
}

// EncodeSyncSnapshotEntries 转换快照条目，最多交付 10000 项和 8 MiB 条目 JSON。
func EncodeSyncSnapshotEntries(entries []domain.SyncSnapshotEntry) (*SyncSnapshotEntriesJSON, error) {
	// 1. 检查条目数，计入整个 JSON 数组的两个方括号。
	if len(entries) > domain.MaxSyncSnapshotEntries {
		return nil, domain.ErrSyncSnapshotTooLarge
	}
	result := &SyncSnapshotEntriesJSON{
		Items:     make([]json.RawMessage, 0, len(entries)),
		SizeBytes: len("[]"),
	}

	// 2. 复用单条转换，按实际编码计入多字节名称、转义字符及分隔符。
	for index, entry := range entries {
		data, err := NewSyncSnapshotEntry(entry)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("编码同步快照条目失败: %w", err)
		}
		if index > 0 {
			result.SizeBytes++
		}
		if len(encoded) > domain.MaxSyncSnapshotBytes-result.SizeBytes {
			return nil, domain.ErrSyncSnapshotTooLarge
		}
		result.SizeBytes += len(encoded)
		result.Items = append(result.Items, json.RawMessage(encoded))
	}

	// 3. 所有条目通过后才交付完整结果。
	return result, nil
}
