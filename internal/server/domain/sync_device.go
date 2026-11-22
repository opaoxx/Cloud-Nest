package domain

import "errors"

// ErrSyncDeviceNotFound 表示归属正确的设备尚未建立同步记录。
var ErrSyncDeviceNotFound = errors.New("设备同步记录不存在")

// ErrInvalidSyncDeviceData 表示同步设备的归属、代次或状态数据异常。
var ErrInvalidSyncDeviceData = errors.New("设备同步数据异常")

// SyncDeviceStatus 表示设备是否已具备可用的同步基线。
type SyncDeviceStatus string

const (
	// SyncDeviceStatusActive 表示设备已确认基线。
	SyncDeviceStatusActive SyncDeviceStatus = "ACTIVE"
	// SyncDeviceStatusResetRequired 表示设备需要确认新基线。
	SyncDeviceStatusResetRequired SyncDeviceStatus = "RESET_REQUIRED"
)

// SyncDeviceState 是锁内读到的设备同步状态，代次和序号按值保存。
type SyncDeviceState struct {
	Status      SyncDeviceStatus
	Generation  SyncCursorGeneration
	AckSequence uint64
}
