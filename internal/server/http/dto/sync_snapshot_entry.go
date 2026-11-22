package dto

import (
	"encoding/hex"
	"strconv"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncSnapshotEntry 是一致性快照中一条普通目录项的公开表示。
type SyncSnapshotEntry struct {
	EntryID   string  `json:"entry_id"`
	ParentID  string  `json:"parent_id"`
	Name      string  `json:"name"`
	EntryType string  `json:"entry_type"`
	Revision  string  `json:"revision"`
	SizeBytes uint64  `json:"size_bytes"`
	SHA256    *string `json:"sha256"` // 目录的摘要必须显式编码为 null。
}

// NewSyncSnapshotEntry 将同一读取视图中的可见条目转换为公开字段。
func NewSyncSnapshotEntry(entry domain.SyncSnapshotEntry) (*SyncSnapshotEntry, error) {
	// 1. 核对普通条目的标识、名称、版本及 JSON 数字范围。
	if entry.ID == 0 || entry.ParentID == 0 || entry.ID == entry.ParentID || entry.Revision == 0 ||
		entry.Name == "" || !utf8.ValidString(entry.Name) || entry.FileSize > maxJSONSafeInteger {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 目录保留空摘要，文件把已校验的 32 字节转换为小写十六进制。
	var digest *string
	switch entry.EntryType {
	case "DIR":
		if entry.FileSize != 0 || entry.VerifiedSHA256 != nil {
			return nil, domain.ErrInvalidFileEntryData
		}
	case "FILE":
		if entry.VerifiedSHA256 == nil {
			return nil, domain.ErrInvalidFileEntryData
		}
		encodedDigest := hex.EncodeToString(entry.VerifiedSHA256[:])
		digest = &encodedDigest
	default:
		return nil, domain.ErrInvalidFileEntryData
	}

	// 3. 以十进制字符串交付标识和版本，保留完整 uint64 精度。
	return &SyncSnapshotEntry{
		EntryID:   strconv.FormatUint(entry.ID, 10),
		ParentID:  strconv.FormatUint(entry.ParentID, 10),
		Name:      entry.Name,
		EntryType: entry.EntryType,
		Revision:  strconv.FormatUint(entry.Revision, 10),
		SizeBytes: entry.FileSize,
		SHA256:    digest,
	}, nil
}
