package main

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerSyncMaintenanceRoundOrdersStagesAndPreservesProgress(t *testing.T) {
	offlineFailure := errors.New("离线重建后续页失败")
	pruneFailure := errors.New("日志裁剪后续页失败")
	deviceFailure := errors.New("单设备标记失败")
	for _, fixture := range []struct {
		name        string
		cancelEarly bool
		cancelAfter bool
		offlineErr  error
		pruneErr    error
		wantErr     error
		wantCalls   []string
	}{
		{name: "single device failure permits locked review", wantCalls: []string{"offline", "prune"}},
		{name: "offline scan failure skips pruning", offlineErr: offlineFailure, wantErr: offlineFailure, wantCalls: []string{"offline"}},
		{name: "cancellation after offline skips pruning", cancelAfter: true, wantErr: context.Canceled, wantCalls: []string{"offline"}},
		{name: "pre-cancellation keeps input cursors", cancelEarly: true, wantErr: context.Canceled},
		{name: "prune scan failure preserves both positions", pruneErr: pruneFailure, wantErr: pruneFailure, wantCalls: []string{"offline", "prune"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if fixture.cancelEarly {
				cancel()
			}
			offline := service.OfflineSyncDeviceBatchResult{Scanned: 2, Marked: 1, NextAfterDeviceID: 8,
				CandidateErrors: []service.OfflineSyncDeviceCandidateError{{UserID: 12, DeviceID: 7, Err: deviceFailure}}}
			pruned := service.SyncChangePruneBatchResult{Scanned: 2, PrunedAccounts: 1, DeletedEvents: 2, NextAfterUserID: 14}
			var calls []string
			got, err := runServerSyncMaintenanceRound(ctx, 6, 11,
				func(received context.Context, after uint64) (service.OfflineSyncDeviceBatchResult, error) {
					if received != ctx || after != 6 {
						t.Fatalf("离线重建未收到原上下文与设备游标：%d", after)
					}
					calls = append(calls, "offline")
					if fixture.cancelAfter {
						cancel()
					}
					return offline, fixture.offlineErr
				},
				func(received context.Context, after uint64) (service.SyncChangePruneBatchResult, error) {
					if received != ctx || after != 11 || !slices.Equal(calls, []string{"offline"}) {
						t.Fatalf("裁剪顺序或输入不符：%d，%v", after, calls)
					}
					calls = append(calls, "prune")
					return pruned, fixture.pruneErr
				},
			)
			want := syncMaintenanceRoundResult{
				Offline: service.OfflineSyncDeviceBatchResult{NextAfterDeviceID: 6},
				Prune:   service.SyncChangePruneBatchResult{NextAfterUserID: 11},
			}
			if len(fixture.wantCalls) > 0 {
				want.Offline = offline
			}
			if len(fixture.wantCalls) == 2 {
				want.PruneStarted = true
				want.Prune = pruned
			}
			if !errors.Is(err, fixture.wantErr) || !slices.Equal(calls, fixture.wantCalls) || !reflect.DeepEqual(got, want) {
				t.Fatalf("编排结果不符：%+v，错误 %v，阶段 %v，期望 %+v", got, err, calls, want)
			}
		})
	}
}
