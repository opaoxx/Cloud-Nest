package domain

import "time"

// MaxOfflineSyncDeviceCandidates 限定一次离线设备扫描返回的数量。
const MaxOfflineSyncDeviceCandidates = 100

// OfflineSyncDeviceCandidate 保存交给后续事务复核的设备归属、代次和同步活动时间。
type OfflineSyncDeviceCandidate struct {
	UserID     uint64
	DeviceID   uint64
	Generation SyncCursorGeneration
	LastSeenAt time.Time
}
