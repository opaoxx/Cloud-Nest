package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func reserveSyncSnapshotForTest(t *testing.T, capacity *SyncSnapshotCapacity) *SyncSnapshotReservation {
	t.Helper()
	reservation, err := capacity.ReserveBuild(t.Context())
	if err != nil || reservation == nil {
		t.Fatalf("申请快照构建名额失败，错误=%v", err)
	}
	t.Cleanup(reservation.Release)
	return reservation
}

func assertSyncSnapshotBusy(t *testing.T, capacity *SyncSnapshotCapacity) {
	t.Helper()
	if reservation, err := capacity.ReserveBuild(t.Context()); reservation != nil || !errors.Is(err, domain.ErrSyncSnapshotBusy) {
		t.Fatalf("容量用尽时应返回繁忙，错误=%v", err)
	}
}

func TestSyncSnapshotCapacityConcurrentBuilds(t *testing.T) {
	// 1. 同时申请，保留所有成功的凭据直到竞争结束。
	var capacity SyncSnapshotCapacity
	const contenders = 16
	start := make(chan struct{})
	type attempt struct {
		reservation *SyncSnapshotReservation
		err         error
	}
	results := make(chan attempt, contenders)
	for range contenders {
		go func() {
			<-start
			reservation, err := capacity.ReserveBuild(t.Context())
			results <- attempt{reservation: reservation, err: err}
		}()
	}
	close(start)
	var admitted []*SyncSnapshotReservation
	for range contenders {
		result := <-results
		if result.reservation != nil && result.err == nil {
			admitted = append(admitted, result.reservation)
			t.Cleanup(result.reservation.Release)
		} else if result.reservation != nil || !errors.Is(result.err, domain.ErrSyncSnapshotBusy) {
			t.Fatalf("未获名额时应返回繁忙，错误=%v", result.err)
		}
	}
	if len(admitted) != 2 {
		t.Fatalf("16 个竞争者中应有 2 个构建获准，实际=%d", len(admitted))
	}

	// 2. 完成一次构建会归还构建名额，保留快照槽位。
	if err := admitted[0].Complete(2); err != nil {
		t.Fatal(err)
	}
	reserveSyncSnapshotForTest(t, &capacity)
	assertSyncSnapshotBusy(t, &capacity)
}

func TestSyncSnapshotCapacitySharedSlots(t *testing.T) {
	// 1. 用 31 个小快照和 1 个构建填满共享槽位。
	var capacity SyncSnapshotCapacity
	ready := make([]*SyncSnapshotReservation, 0, 31)
	for range 31 {
		reservation := reserveSyncSnapshotForTest(t, &capacity)
		if err := reservation.Complete(2); err != nil {
			t.Fatal(err)
		}
		ready = append(ready, reservation)
	}
	lastSlot := reserveSyncSnapshotForTest(t, &capacity)
	assertSyncSnapshotBusy(t, &capacity)

	// 2. 完成构建后槽位仍占用。移除保留项后可以复用一个槽位。
	if err := lastSlot.Complete(2); err != nil {
		t.Fatal(err)
	}
	assertSyncSnapshotBusy(t, &capacity)
	ready[0].Release()
	reserveSyncSnapshotForTest(t, &capacity)
	assertSyncSnapshotBusy(t, &capacity)
}

func TestSyncSnapshotCapacitySharedBytes(t *testing.T) {
	// 1. 先保留 56 MiB 减 2 字节，额外构建仍须预留完整的 8 MiB。
	var capacity SyncSnapshotCapacity
	const maxItemBytes = 8 * 1024 * 1024
	for range 6 {
		if err := reserveSyncSnapshotForTest(t, &capacity).Complete(maxItemBytes); err != nil {
			t.Fatal(err)
		}
	}
	if err := reserveSyncSnapshotForTest(t, &capacity).Complete(maxItemBytes - 2); err != nil {
		t.Fatal(err)
	}
	lastBuild := reserveSyncSnapshotForTest(t, &capacity)
	assertSyncSnapshotBusy(t, &capacity)

	// 2. 结算为 2 字节后再申请一份，恰好用满 64 MiB。
	if err := lastBuild.Complete(2); err != nil {
		t.Fatal(err)
	}
	fullBudget := reserveSyncSnapshotForTest(t, &capacity)
	if err := fullBudget.Complete(maxItemBytes); err != nil {
		t.Fatal(err)
	}
	assertSyncSnapshotBusy(t, &capacity)

	// 3. 移除 2 字节不足以再建一份，归还一份大快照后才接受。
	lastBuild.Release()
	assertSyncSnapshotBusy(t, &capacity)
	fullBudget.Release()
	reserveSyncSnapshotForTest(t, &capacity)
}

func TestSyncSnapshotCapacityRelease(t *testing.T) {
	// 1. 已取消的申请拒绝。结算输入异常时，失败路径仍须释放原预留。
	var capacity SyncSnapshotCapacity
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if reservation, err := capacity.ReserveBuild(canceled); reservation != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("取消时应拒绝申请，错误=%v", err)
	}
	failed := reserveSyncSnapshotForTest(t, &capacity)
	if err := failed.Complete(1); err == nil {
		t.Fatal("小于空 JSON 数组的大小应拒绝")
	}
	if err := failed.Complete(domain.MaxSyncSnapshotBytes + 1); !errors.Is(err, domain.ErrSyncSnapshotTooLarge) {
		t.Fatalf("超限结算应返回快照过大，错误=%v", err)
	}
	other := reserveSyncSnapshotForTest(t, &capacity)
	assertSyncSnapshotBusy(t, &capacity)

	// 2. 并发释放同一次预留的不同引用，只能归还同一份资源。
	copied := *failed
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(failed.Release)
		wait.Go(copied.Release)
	}
	wait.Wait()
	if err := failed.Complete(2); err == nil {
		t.Fatal("已释放的预留不得再结算")
	}
	newBuild := reserveSyncSnapshotForTest(t, &capacity)
	assertSyncSnapshotBusy(t, &capacity)
	if err := other.Complete(2); err != nil {
		t.Fatal(err)
	}
	if err := other.Complete(2); err == nil {
		t.Fatal("同一构建不得重复结算")
	}
	other.Release()
	newBuild.Release()
	if capacity.activeBuilds != 0 || capacity.usedSlots != 0 || capacity.usedBytes != 0 {
		t.Fatalf("资源未完全归还，构建=%d，槽位=%d，字节=%d", capacity.activeBuilds, capacity.usedSlots, capacity.usedBytes)
	}
}
