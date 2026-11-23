package domain

import "encoding/json"

// SyncChangeEvent 是增量拉取读视图中的一条已提交变更记录。
type SyncChangeEvent struct {
	Sequence  uint64
	EntryID   uint64
	Revision  uint64
	EventType string
	Payload   json.RawMessage
}
