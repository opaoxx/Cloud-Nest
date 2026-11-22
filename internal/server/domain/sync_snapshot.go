package domain

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
)

// MaxSyncSnapshotEntries 限定一份同步快照的条目数，不计独立返回的根目录。
const MaxSyncSnapshotEntries = 10000

// MaxSyncSnapshotBytes 限定一份快照的条目 JSON 数组字节数，包含方括号和逗号。
const MaxSyncSnapshotBytes = 8 * 1024 * 1024

// MaxSyncSnapshotTokenLength 限定快照分页及基线凭据的公开文本长度。
const MaxSyncSnapshotTokenLength = 1024

// ErrSyncSnapshotTooLarge 表示完整快照超出允许的大小。
var ErrSyncSnapshotTooLarge = errors.New("同步快照超过大小上限")

// ErrSyncSnapshotBusy 表示进程当前无法为新的快照构建预留资源。
var ErrSyncSnapshotBusy = errors.New("同步快照构建繁忙，请稍后重试")

// ErrInvalidSyncSnapshotCursor 表示分页凭据或其快照绑定不匹配。
var ErrInvalidSyncSnapshotCursor = errors.New("同步快照分页凭据无效")

// ErrSyncSnapshotExpired 表示可信设备的快照已到期、丢失或代次已变更。
var ErrSyncSnapshotExpired = errors.New("同步快照已失效，请重新建立基线")

// SyncCursorGeneration 是同步设备持久化的 16 字节随机代次，按完整值比较。
type SyncCursorGeneration [16]byte

// SyncSnapshotHeader 保存账号一致性快照的根目录和变更截止位置。
type SyncSnapshotHeader struct {
	// RootEntryID 是账号真实活动根目录的 ID。
	RootEntryID uint64
	// HighWaterSequence 是同一读取视图中的 users.change_seq，允许为零。
	HighWaterSequence uint64
}

// SyncSnapshotEntriesJSON 保存可按位置分页的条目 JSON 和整份数组的字节数。
type SyncSnapshotEntriesJSON struct {
	// Items 保持输入条目顺序，空树编码为 []。
	Items []json.RawMessage
	// SizeBytes 包含条目、方括号和逗号，可用于整份快照的预算检查。
	SizeBytes int
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
