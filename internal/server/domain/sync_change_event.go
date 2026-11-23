package domain

import (
	"encoding/json"
	"errors"
)

// ErrInvalidSyncChangePage 表示服务端读取的增量页或其读取范围异常。
var ErrInvalidSyncChangePage = errors.New("增量变更页异常")

// SyncChangeEvent 是增量拉取读视图中的一条已提交变更记录。
type SyncChangeEvent struct {
	Sequence  uint64
	EntryID   uint64
	Revision  uint64
	EventType string
	Payload   json.RawMessage
}
