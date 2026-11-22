package domain

import (
	"crypto/sha256"
	"errors"
)

// MaxSyncSnapshotEntries 限定一份同步快照的条目数，不计独立返回的根目录。
const MaxSyncSnapshotEntries = 10000

// ErrSyncSnapshotTooLarge 表示完整快照超出允许的大小。
var ErrSyncSnapshotTooLarge = errors.New("同步快照超过大小上限")

// SyncSnapshotHeader 保存账号一致性快照的根目录和变更截止位置。
type SyncSnapshotHeader struct {
	// RootEntryID 是账号真实活动根目录的 ID。
	RootEntryID uint64
	// HighWaterSequence 是同一读取视图中的 users.change_seq，允许为零。
	HighWaterSequence uint64
}

// SyncSnapshotEntry 保存一个可见后代的目录元数据和已验证内容摘要。
type SyncSnapshotEntry struct {
	// ID 是条目主键，ParentID 指向根目录或同一快照中的目录。
	ID       uint64
	ParentID uint64
	// Name 和 EntryType 保存展示名称与 FILE 或 DIR 类型。
	Name      string
	EntryType string
	// Revision 和 FileSize 保存目录项版本与逻辑字节数。
	Revision uint64
	FileSize uint64
	// VerifiedSHA256 是文件的服务端校验摘要，目录为 nil。
	VerifiedSHA256 *[sha256.Size]byte
}
