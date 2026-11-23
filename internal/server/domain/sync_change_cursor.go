package domain

import "errors"

// ErrInvalidSyncChangeCursor 表示增量游标的身份、代次或凭据数据无效。
var ErrInvalidSyncChangeCursor = errors.New("增量拉取游标无效")

// SyncChangeCursorPosition 保存同一设备代次中最后交付的位置，序号允许为零。
type SyncChangeCursorPosition struct {
	UserID     uint64
	DeviceID   uint64
	Generation SyncCursorGeneration
	Sequence   uint64
}
