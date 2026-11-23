package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestVerifyRestoredSyncDeviceResetComparesEntireRange(t *testing.T) {
	originalState := domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: domain.SyncCursorGeneration{0xaa}}
	imported := []domain.RestoredSyncDeviceCandidate{
		{UserID: 1, DeviceID: 11, State: originalState},
		{UserID: 1, DeviceID: 22, State: originalState},
		{UserID: 2, DeviceID: 40, State: domain.SyncDeviceState{Status: domain.SyncDeviceStatusResetRequired, Generation: originalState.Generation}},
		{UserID: 2, DeviceID: 70, State: originalState},
	}
	reset := slices.Clone(imported)
	for index := range reset {
		reset[index].State.Status = domain.SyncDeviceStatusResetRequired
		reset[index].State.Generation[15] = 1
	}
	missingMiddle := append(slices.Clone(reset[:1]), reset[2:]...)
	extra := reset[3]
	extra.DeviceID = 71
	extraAfterFullPage := append(slices.Clone(reset), extra)
	wrongOwner := slices.Clone(reset)
	wrongOwner[1].UserID = 2
	oldPending := slices.Clone(reset)
	oldPending[2] = imported[2]
	activeWithNewGeneration := slices.Clone(reset)
	activeWithNewGeneration[1].State.Status = domain.SyncDeviceStatusActive
	unresetPosition := slices.Clone(reset)
	unresetPosition[2].State.AckSequence = 1
	unordered := slices.Clone(imported)
	unordered[0], unordered[1] = unordered[1], unordered[0]

	for _, fixture := range []struct {
		name     string
		imported []domain.RestoredSyncDeviceCandidate
		current  []domain.RestoredSyncDeviceCandidate
		want     int
		wantErr  error
		wantRead []uint64
	}{
		{name: "all rows and final empty page", imported: imported, current: reset, want: 4, wantRead: []uint64{0, 22, 70}},
		{name: "explicit empty import", imported: []domain.RestoredSyncDeviceCandidate{}, wantRead: []uint64{0}},
		{name: "missing intermediate row", imported: imported, current: missingMiddle, want: 1, wantErr: ErrRestoredSyncDeviceResetIncomplete, wantRead: []uint64{0}},
		{name: "missing last row", imported: imported, current: reset[:3], want: 3, wantErr: ErrRestoredSyncDeviceResetIncomplete, wantRead: []uint64{0, 22}},
		{name: "extra row beyond full last page", imported: imported, current: extraAfterFullPage, want: 4, wantErr: ErrRestoredSyncDeviceResetIncomplete, wantRead: []uint64{0, 22, 70}},
		{name: "wrong account", imported: imported, current: wrongOwner, want: 1, wantErr: ErrRestoredSyncDeviceResetIncomplete, wantRead: []uint64{0}},
		{name: "old generation on pending device", imported: imported, current: oldPending, want: 2, wantErr: ErrRestoredSyncDeviceResetIncomplete, wantRead: []uint64{0, 22}},
		{name: "active despite new generation", imported: imported, current: activeWithNewGeneration, want: 1, wantErr: ErrRestoredSyncDeviceResetIncomplete, wantRead: []uint64{0}},
		{name: "nonzero pending position", imported: imported, current: unresetPosition, want: 2, wantErr: domain.ErrInvalidSyncDeviceData, wantRead: []uint64{0, 22}},
		{name: "unordered import", imported: unordered, current: reset, wantErr: domain.ErrInvalidSyncDeviceData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var cursors []uint64
			verified, err := VerifyRestoredSyncDeviceReset(t.Context(), fixture.imported, 2,
				func(_ context.Context, after uint64, limit int) ([]domain.RestoredSyncDeviceCandidate, error) {
					if limit != 2 {
						t.Fatalf("目标页大小不符：%d", limit)
					}
					cursors = append(cursors, after)
					index := 0
					for index < len(fixture.current) && fixture.current[index].DeviceID <= after {
						index++
					}
					return fixture.current[index:min(index+limit, len(fixture.current))], nil
				})
			if verified != fixture.want || !errors.Is(err, fixture.wantErr) || !slices.Equal(cursors, fixture.wantRead) {
				t.Fatalf("复核计数、错误或分页不符：%d，%v，%v", verified, err, cursors)
			}
		})
	}
}

func TestVerifyRestoredSyncDeviceResetPreservesErrors(t *testing.T) {
	row := domain.RestoredSyncDeviceCandidate{UserID: 7, DeviceID: 11,
		State: domain.SyncDeviceState{Status: domain.SyncDeviceStatusResetRequired, Generation: domain.SyncCursorGeneration{1}}}
	imported := []domain.RestoredSyncDeviceCandidate{row}
	current := row
	current.State.Generation[15] = 2
	readFailure := errors.New("合成恢复页失败")
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		expectedError := readFailure
		if canceled {
			expectedError = context.Canceled
		}
		got, err := VerifyRestoredSyncDeviceReset(ctx, imported, 1,
			func(_ context.Context, after uint64, _ int) ([]domain.RestoredSyncDeviceCandidate, error) {
				if after == 0 {
					return []domain.RestoredSyncDeviceCandidate{current}, nil
				}
				if canceled {
					cancel()
					return nil, nil
				}
				return nil, readFailure
			},
		)
		cancel()
		if got != 1 || !errors.Is(err, expectedError) {
			t.Fatalf("读取最后一页失败时误报复核完成：%d，%v", got, err)
		}
	}
	noRead := func(context.Context, uint64, int) ([]domain.RestoredSyncDeviceCandidate, error) {
		t.Fatal("无效参数进入分页")
		return nil, nil
	}
	if got, err := VerifyRestoredSyncDeviceReset(t.Context(), nil, 1, noRead); got != 0 || err == nil {
		t.Fatal("缺少导入清单的复核返回成功")
	}
	if got, err := VerifyRestoredSyncDeviceReset(t.Context(), imported, 0, noRead); got != 0 || err == nil {
		t.Fatal("非法页大小的复核返回成功")
	}
}
