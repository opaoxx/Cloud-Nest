package domain

import "errors"

// ErrSyncChangePruneCountMismatch 表示实际删除的日志数与事务内选出的范围不符。
var ErrSyncChangePruneCountMismatch = errors.New("同步日志裁剪行数不一致")

// SyncChangePruneRange 表示一批连续的变更事件序号，零值表示空批次。
type SyncChangePruneRange struct {
	FirstSequence uint64
	LastSequence  uint64
	Count         int
}
