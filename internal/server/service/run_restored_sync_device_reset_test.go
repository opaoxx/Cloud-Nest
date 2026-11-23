package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestRunRestoredSyncDeviceResetStopsBeforeWrite(t *testing.T) {
	fixtureError := errors.New("合成清单故障")
	for _, fixture := range []struct {
		name       string
		readErr    error
		saveErr    error
		cancelSave bool
		wantErr    error
		wantSaves  int
	}{
		{name: "read fails", readErr: fixtureError, wantErr: fixtureError},
		{name: "save fails", saveErr: fixtureError, wantErr: fixtureError, wantSaves: 1},
		{name: "canceled after save", cancelSave: true, wantErr: context.Canceled, wantSaves: 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			saves := 0
			result, err := RunRestoredSyncDeviceReset(ctx, nil, 2, 2,
				func(context.Context, uint64, int) ([]domain.RestoredSyncDeviceCandidate, error) {
					return nil, fixture.readErr
				},
				func(context.Context, domain.RestoredSyncDeviceCandidate) error {
					t.Fatal("清单失败或取消后进入写入")
					return nil
				},
				func(_ context.Context, roster []domain.RestoredSyncDeviceCandidate) error {
					saves++
					if roster == nil || len(roster) != 0 {
						t.Fatalf("空目标应保存明确的空清单：%+v", roster)
					}
					if fixture.cancelSave {
						cancel()
					}
					return fixture.saveErr
				})
			if result != (RestoredSyncDeviceResetResult{}) || !errors.Is(err, fixture.wantErr) || saves != fixture.wantSaves {
				t.Fatalf("失败应停止流程：%+v，%v，清单保存次数 %d", result, err, saves)
			}
		})
	}
}

func TestRunRestoredSyncDeviceResetRejectsChangedScope(t *testing.T) {
	state := domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: domain.SyncCursorGeneration{1}}
	original := []domain.RestoredSyncDeviceCandidate{{UserID: 7, DeviceID: 10, State: state}, {UserID: 7, DeviceID: 20, State: state}}
	wrongOwner := slices.Clone(original)
	wrongOwner[1].UserID = 8
	for _, fixture := range []struct {
		name    string
		current []domain.RestoredSyncDeviceCandidate
	}{
		{name: "missing row", current: original[:1]},
		{name: "unexpected row", current: append(slices.Clone(original), domain.RestoredSyncDeviceCandidate{UserID: 7, DeviceID: 21, State: state})},
		{name: "wrong account", current: wrongOwner},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			result, err := RunRestoredSyncDeviceReset(t.Context(), original, 3, 3,
				func(context.Context, uint64, int) ([]domain.RestoredSyncDeviceCandidate, error) {
					return fixture.current, nil
				},
				func(context.Context, domain.RestoredSyncDeviceCandidate) error {
					t.Fatal("范围不匹配的页进入换代")
					return nil
				},
				nil)
			if result != (RestoredSyncDeviceResetResult{}) || !errors.Is(err, ErrRestoredSyncDeviceResetIncomplete) {
				t.Fatalf("范围不匹配应被拒绝：%+v，%v", result, err)
			}
		})
	}
}

func TestRunRestoredSyncDeviceResetRequiresReadback(t *testing.T) {
	original := []domain.RestoredSyncDeviceCandidate{{UserID: 7, DeviceID: 10,
		State: domain.SyncDeviceState{Status: domain.SyncDeviceStatusResetRequired, Generation: domain.SyncCursorGeneration{1}}}}
	list := func(_ context.Context, after uint64, _ int) ([]domain.RestoredSyncDeviceCandidate, error) {
		if after == 0 {
			return original, nil
		}
		return nil, nil
	}
	resets := 0
	reset := func(context.Context, domain.RestoredSyncDeviceCandidate) error {
		resets++
		return nil
	}
	result, err := RunRestoredSyncDeviceReset(t.Context(), original, 1, 1, list, reset, nil)
	if result != (RestoredSyncDeviceResetResult{Reset: 1}) || !errors.Is(err, ErrRestoredSyncDeviceResetIncomplete) || resets != 1 {
		t.Fatalf("仅扫描到末尾不能通过：%+v，%v，写入次数 %d", result, err, resets)
	}
	emptyList := func(context.Context, uint64, int) ([]domain.RestoredSyncDeviceCandidate, error) {
		return nil, nil
	}
	empty := []domain.RestoredSyncDeviceCandidate{}
	result, err = RunRestoredSyncDeviceReset(t.Context(), empty, 1, 1, emptyList, reset, nil)
	if result != (RestoredSyncDeviceResetResult{}) || err != nil || resets != 1 {
		t.Fatalf("明确的空目标应通过复核：%+v，%v，写入次数 %d", result, err, resets)
	}
	result, err = RunRestoredSyncDeviceReset(t.Context(), nil, 1, 1, emptyList, reset, nil)
	if result != (RestoredSyncDeviceResetResult{}) || err == nil || resets != 1 {
		t.Fatalf("缺少原始清单和首次保存方法应失败：%+v，%v，写入次数 %d", result, err, resets)
	}
}
