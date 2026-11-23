package domain

import "errors"

// ErrRestoredSyncDeviceCandidateStale 表示恢复同步记录与扫描时的状态不同。
var ErrRestoredSyncDeviceCandidateStale = errors.New("恢复同步设备候选状态已变化")

// MaxRestoredSyncDeviceCandidates 限定一页最多读取的恢复同步设备数。
const MaxRestoredSyncDeviceCandidates = 100

// RestoredSyncDeviceCandidate 保存恢复目标上待加锁复核的同步记录。
type RestoredSyncDeviceCandidate struct {
	UserID   uint64
	DeviceID uint64
	State    SyncDeviceState
}
