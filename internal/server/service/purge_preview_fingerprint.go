package service

import (
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// purgePreviewFingerprintEntry 是指纹第 1 版使用的固定字段顺序。
type purgePreviewFingerprintEntry struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
	Status   string `json:"status"`
}

// BuildPurgePreviewSubtreeFingerprint 生成保留目录项的稳定子树指纹。
// entries 包含预检目标和全部保留后代，输入切片保持原顺序。
func BuildPurgePreviewSubtreeFingerprint(entries []domain.PurgePreviewSubtreeEntry) ([sha256.Size]byte, error) {
	// 1. 检查范围，复制记录并按目录项 ID 升序排列。
	if len(entries) == 0 {
		return [sha256.Size]byte{}, domain.ErrInvalidFileEntryData
	}
	if len(entries) > domain.MaxFileEntrySubtreeNodes {
		return [sha256.Size]byte{}, domain.ErrFileEntrySubtreeTooLarge
	}
	ordered := slices.Clone(entries)
	slices.SortFunc(ordered, func(first, second domain.PurgePreviewSubtreeEntry) int {
		return cmp.Compare(first.ID, second.ID)
	})

	// 2. 校验记录，按固定格式编码 ID、版本和状态。
	payload := struct {
		Version uint8                          `json:"version"`
		Entries []purgePreviewFingerprintEntry `json:"entries"`
	}{
		Version: 1,
		Entries: make([]purgePreviewFingerprintEntry, len(ordered)),
	}
	for index, entry := range ordered {
		if !entry.IsValid() || (index > 0 && ordered[index-1].ID == entry.ID) {
			return [sha256.Size]byte{}, domain.ErrInvalidFileEntryData
		}
		payload.Entries[index] = purgePreviewFingerprintEntry{
			ID:       strconv.FormatUint(entry.ID, 10),
			Revision: strconv.FormatUint(entry.Revision, 10),
			Status:   entry.Status,
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("编码永久清理子树指纹失败: %w", err)
	}

	// 3. 计算 SHA-256，返回可复算的子树指纹。
	return sha256.Sum256(encoded), nil
}

// buildPurgePreviewTargetFingerprint 确认子树包含完整且一致的目标记录。
func buildPurgePreviewTargetFingerprint(target *domain.FileEntry, entries []domain.PurgePreviewSubtreeEntry) ([sha256.Size]byte, error) {
	expectedTarget := domain.PurgePreviewSubtreeEntry{
		ID: target.ID, EntryType: target.EntryType, Status: target.Status,
		Revision: target.Revision, FileSize: target.FileSize,
	}
	if !slices.Contains(entries, expectedTarget) {
		return [sha256.Size]byte{}, domain.ErrInvalidFileEntryData
	}
	return BuildPurgePreviewSubtreeFingerprint(entries)
}
