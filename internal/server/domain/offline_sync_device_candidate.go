package domain

import (
	"errors"
	"time"
)

// MaxOfflineSyncDeviceCandidates 限定一次离线设备扫描返回的数量。
const MaxOfflineSyncDeviceCandidates = 100

// ErrOfflineSyncDeviceCandidateStale 表示候选已不满足本轮离线重建条件。
var ErrOfflineSyncDeviceCandidateStale = errors.New("离线同步设备候选已失效")

// OfflineSyncDeviceCandidate 保存交给后续事务复核的设备归属、代次和同步活动时间。
type OfflineSyncDeviceCandidate struct {
	UserID     uint64
	DeviceID   uint64
	Generation SyncCursorGeneration
	LastSeenAt time.Time
}

// IsValid 核对候选的身份、完整代次和微秒精度同步活动时间。
func (candidate OfflineSyncDeviceCandidate) IsValid() bool {
	return candidate.UserID != 0 && candidate.DeviceID != 0 &&
		candidate.Generation != (SyncCursorGeneration{}) &&
		!candidate.LastSeenAt.IsZero() && candidate.LastSeenAt.Nanosecond()%int(time.Microsecond) == 0
}
