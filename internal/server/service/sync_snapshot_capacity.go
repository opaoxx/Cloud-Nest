package service

import (
	"context"
	"fmt"
	"sync"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const (
	maxConcurrentSyncSnapshotBuilds = 2
	maxSyncSnapshotSlots            = 32
	syncSnapshotCapacityBytes       = 64 * 1024 * 1024
)

// SyncSnapshotCapacity 保存同一进程快照构建和保留数据共用的预算，零值可用。
// 使用后须通过指针共享实例。锁只保护计数及预留状态，构建和读写在锁外执行。
type SyncSnapshotCapacity struct {
	mutex        sync.Mutex
	activeBuilds int
	usedSlots    int
	usedBytes    int
}

// SyncSnapshotReservation 是一次构建的名额与预算凭据，复制后仍指向同一预留。
type SyncSnapshotReservation struct {
	state *syncSnapshotReservationState
}

type syncSnapshotReservationState struct {
	capacity  *SyncSnapshotCapacity
	building  bool
	sizeBytes int // 零表示预留已释放。
}

// ReserveBuild 原子预留一个构建名额、一个槽位和最大单份快照字节数。
// 构建失败或取消后由调用方释放。成功保留快照时先用 Complete 结算实际大小。
func (capacity *SyncSnapshotCapacity) ReserveBuild(ctx context.Context) (*SyncSnapshotReservation, error) {
	// 1. 核对上下文，在同一把锁内检查三项剩余资源。
	if capacity == nil || ctx == nil {
		return nil, fmt.Errorf("预留同步快照需要容量实例和上下文")
	}
	capacity.mutex.Lock()
	defer capacity.mutex.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if capacity.activeBuilds >= maxConcurrentSyncSnapshotBuilds || capacity.usedSlots >= maxSyncSnapshotSlots ||
		domain.MaxSyncSnapshotBytes > syncSnapshotCapacityBytes-capacity.usedBytes {
		return nil, domain.ErrSyncSnapshotBusy
	}

	// 2. 同时更新三个计数，返回可结算或释放的预留凭据。
	capacity.activeBuilds++
	capacity.usedSlots++
	capacity.usedBytes += domain.MaxSyncSnapshotBytes
	return &SyncSnapshotReservation{state: &syncSnapshotReservationState{
		capacity: capacity, building: true, sizeBytes: domain.MaxSyncSnapshotBytes,
	}}, nil
}

// Complete 将构建预留转为实际 JSON 大小，本次槽位继续保留到 Release。
func (reservation *SyncSnapshotReservation) Complete(sizeBytes int) error {
	// 1. 校验实际数组大小，空数组至少有两个字节。
	if reservation == nil || reservation.state == nil || sizeBytes < len("[]") {
		return fmt.Errorf("结算同步快照需要有效预留和 JSON 数组大小")
	}
	if sizeBytes > domain.MaxSyncSnapshotBytes {
		return domain.ErrSyncSnapshotTooLarge
	}

	// 2. 在锁内复核仍在构建，归还构建名额和多预留的字节数。
	state := reservation.state
	capacity := state.capacity
	capacity.mutex.Lock()
	defer capacity.mutex.Unlock()
	if !state.building {
		return fmt.Errorf("同步快照预留已结算或释放")
	}
	capacity.activeBuilds--
	capacity.usedBytes -= state.sizeBytes - sizeBytes
	state.building = false
	state.sizeBytes = sizeBytes
	return nil
}

// Release 归还该预留占用的资源，可并发重复调用。
func (reservation *SyncSnapshotReservation) Release() {
	// 1. 加锁找到尚未释放的预留。
	if reservation == nil || reservation.state == nil {
		return
	}
	state := reservation.state
	capacity := state.capacity
	capacity.mutex.Lock()
	defer capacity.mutex.Unlock()
	if state.sizeBytes == 0 {
		return
	}

	// 2. 归还槽位和字节。尚在构建时，同时归还构建名额。
	if state.building {
		capacity.activeBuilds--
		state.building = false
	}
	capacity.usedSlots--
	capacity.usedBytes -= state.sizeBytes
	state.sizeBytes = 0
}
