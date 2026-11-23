package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestRestoredSyncDeviceBatchStopsAtUnconfirmedDevice(t *testing.T) {
	writeFailure := errors.New("合成写入失败")
	unknownCommit := errors.Join(domain.ErrUserTransactionCommitUnconfirmed, context.Canceled)
	for _, fixture := range []struct {
		name       string
		resetError error
		cancel     bool
		wantError  error
		want       RestoredSyncDeviceBatchResult
	}{
		{name: "stops on database failure", resetError: writeFailure, wantError: writeFailure,
			want: RestoredSyncDeviceBatchResult{Scanned: 3, Reset: 1, NextAfterDeviceID: 21, FailedUserID: 2, FailedDeviceID: 22}},
		{name: "stale candidate is not skipped", resetError: domain.ErrRestoredSyncDeviceCandidateStale, wantError: domain.ErrRestoredSyncDeviceCandidateStale,
			want: RestoredSyncDeviceBatchResult{Scanned: 3, Reset: 1, NextAfterDeviceID: 21, FailedUserID: 2, FailedDeviceID: 22}},
		{name: "unknown commit stops at prior position", resetError: unknownCommit, cancel: true, wantError: domain.ErrUserTransactionCommitUnconfirmed,
			want: RestoredSyncDeviceBatchResult{Scanned: 3, Reset: 1, NextAfterDeviceID: 21, FailedUserID: 2, FailedDeviceID: 22}},
		{name: "cancellation before commit", resetError: context.Canceled, cancel: true, wantError: context.Canceled,
			want: RestoredSyncDeviceBatchResult{Scanned: 3, Reset: 1, NextAfterDeviceID: 21, FailedUserID: 2, FailedDeviceID: 22}},
		{name: "cancellation after commit", cancel: true, wantError: context.Canceled,
			want: RestoredSyncDeviceBatchResult{Scanned: 3, Reset: 2, NextAfterDeviceID: 22}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			state := domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: domain.SyncCursorGeneration{1}}
			var attempts []uint64
			batch := NewRestoredSyncDeviceBatch(
				func(listContext context.Context, after uint64, limit int) ([]domain.RestoredSyncDeviceCandidate, error) {
					if listContext != ctx || after != 20 || limit != 3 {
						t.Fatalf("分页参数不符：%d，%d", after, limit)
					}
					return []domain.RestoredSyncDeviceCandidate{
						{UserID: 1, DeviceID: 21, State: state},
						{UserID: 2, DeviceID: 22, State: state},
						{UserID: 2, DeviceID: 23, State: state},
					}, nil
				},
				func(resetContext context.Context, candidate domain.RestoredSyncDeviceCandidate) error {
					if resetContext != ctx {
						t.Fatal("换代未得到原上下文")
					}
					attempts = append(attempts, candidate.DeviceID)
					if candidate.DeviceID == 22 {
						if fixture.cancel {
							cancel()
						}
						return fixture.resetError
					}
					return nil
				},
			)
			got, err := batch.RunOnce(ctx, 20, 4, 3)
			if !errors.Is(err, fixture.wantError) || got != fixture.want || !slices.Equal(attempts, []uint64{21, 22}) {
				t.Fatalf("批次越过未完成设备或丢失已提交进度：%+v，%v，尝试 %v", got, err, attempts)
			}
		})
	}
}

func TestRestoredSyncDeviceBatchRejectsInvalidPage(t *testing.T) {
	valid := domain.RestoredSyncDeviceCandidate{UserID: 1, DeviceID: 21,
		State: domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: domain.SyncCursorGeneration{1}}}
	outdated := valid
	outdated.DeviceID = 20
	invalid := valid
	invalid.DeviceID = 23
	invalid.State.Generation = domain.SyncCursorGeneration{}
	readFailure := errors.New("读取合成页失败")
	for _, fixture := range []struct {
		name string
		page []domain.RestoredSyncDeviceCandidate
		err  error
		want error
	}{
		{name: "empty target reaches end"},
		{name: "scan fails", err: readFailure, want: readFailure},
		{name: "cursor does not advance", page: []domain.RestoredSyncDeviceCandidate{outdated}, want: domain.ErrInvalidSyncDeviceData},
		{name: "invalid second record rejects entire page", page: []domain.RestoredSyncDeviceCandidate{valid, invalid}, want: domain.ErrInvalidSyncDeviceData},
		{name: "page exceeds limit", page: []domain.RestoredSyncDeviceCandidate{valid, valid, valid}, want: domain.ErrInvalidSyncDeviceData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			listCalls := 0
			batch := NewRestoredSyncDeviceBatch(
				func(context.Context, uint64, int) ([]domain.RestoredSyncDeviceCandidate, error) {
					listCalls++
					return fixture.page, fixture.err
				},
				func(context.Context, domain.RestoredSyncDeviceCandidate) error {
					t.Fatal("无候选或无效页进入了换代事务")
					return nil
				},
			)
			for _, limits := range [][2]int{{0, 2}, {2, 0}, {101, 2}, {2, 101}} {
				if got, err := batch.RunOnce(t.Context(), 20, limits[0], limits[1]); err == nil || got.NextAfterDeviceID != 20 || listCalls != 0 {
					t.Fatalf("非法预算进入扫描：%v，%+v，%v", limits, got, err)
				}
			}
			got, err := batch.RunOnce(t.Context(), 20, 2, 2)
			want := RestoredSyncDeviceBatchResult{NextAfterDeviceID: 20, ReachedEnd: fixture.want == nil}
			if !errors.Is(err, fixture.want) || got != want || listCalls != 1 {
				t.Fatalf("空页或拒绝结果不符：%+v，%v，查询 %d 次", got, err, listCalls)
			}
		})
	}
}
