package service

import (
	"errors"
	"math"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestValidateSyncChangeConfirmation(t *testing.T) {
	// 1. 模拟账号事务中读到的活动设备和不同的日志区间。
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 27}
	generation := domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe}
	window := domain.SyncChangeSequenceBounds{MinimumPosition: 17, HighWaterSequence: 22}
	atEnd := domain.SyncChangeSequenceBounds{MinimumPosition: 22, HighWaterSequence: 22}
	for _, scenario := range []struct {
		name          string
		position, ack uint64
		bounds        domain.SyncChangeSequenceBounds
		want          SyncChangeConfirmationDecision
		wantError     error
	}{
		{name: "advance to delivered position", position: 19, ack: 17, bounds: window,
			want: SyncChangeConfirmationDecision{AckSequence: 19, RecordActivity: true}},
		{name: "equal records sync activity", position: 19, ack: 19, bounds: window,
			want: SyncChangeConfirmationDecision{AckSequence: 19, RecordActivity: true}},
		{name: "older retained position is read only", position: 17, ack: 22, bounds: window,
			want: SyncChangeConfirmationDecision{AckSequence: 22}},
		{name: "inclusive high water", position: 22, ack: 17, bounds: window,
			want: SyncChangeConfirmationDecision{AckSequence: 22, RecordActivity: true}},
		{name: "empty account at zero", want: SyncChangeConfirmationDecision{RecordActivity: true}},
		{name: "no retained events at high water", position: 22, ack: 22, bounds: atEnd,
			want: SyncChangeConfirmationDecision{AckSequence: 22, RecordActivity: true}},
		{name: "full uint64 advance", position: math.MaxUint64, ack: math.MaxUint64 - 1,
			bounds: domain.SyncChangeSequenceBounds{MinimumPosition: math.MaxUint64 - 1, HighWaterSequence: math.MaxUint64},
			want:   SyncChangeConfirmationDecision{AckSequence: math.MaxUint64, RecordActivity: true}},
		{name: "older pruned position is expired", position: 16, ack: 22, bounds: window, wantError: domain.ErrSyncSnapshotExpired},
		{name: "no retained events below high water", position: 21, ack: 22, bounds: atEnd, wantError: domain.ErrSyncSnapshotExpired},
		{name: "cursor above high water", position: 23, ack: 22, bounds: window, wantError: domain.ErrInvalidSyncChangeCursor},
		{name: "stored acknowledgment above high water", position: 23, ack: 24, bounds: window, wantError: domain.ErrInvalidSyncDeviceData},
		{name: "contradictory log boundaries", position: 22, ack: 22,
			bounds: domain.SyncChangeSequenceBounds{MinimumPosition: 23, HighWaterSequence: 22}, wantError: domain.ErrInvalidSyncSnapshotSequence},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			position := domain.SyncChangeCursorPosition{UserID: actor.UserID, DeviceID: actor.DeviceID, Generation: generation, Sequence: scenario.position}
			state := domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: generation, AckSequence: scenario.ack}

			// 2. 核对目标位置和活动标记，异常时必须交付零值决定。
			decision, err := ValidateSyncChangeConfirmation(actor, position, state, scenario.bounds)
			if decision != scenario.want || !errors.Is(err, scenario.wantError) {
				t.Fatalf("确认决定 %+v，错误 %v，预期 %+v 和 %v", decision, err, scenario.want, scenario.wantError)
			}
		})
	}
}

func TestValidateSyncChangeConfirmationRejectsInvalidIdentityOrDevice(t *testing.T) {
	// 1. 使用有效区间，每次只改变认证身份、游标绑定或设备状态。
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 27}
	generation := domain.SyncCursorGeneration{0: 0xf1, 15: 0xfe}
	bounds := domain.SyncChangeSequenceBounds{MinimumPosition: 17, HighWaterSequence: 22}
	for _, scenario := range []struct {
		name      string
		configure func(*domain.Actor, *domain.SyncChangeCursorPosition, *domain.SyncDeviceState)
		wantError error
	}{
		{"missing account", func(actor *domain.Actor, _ *domain.SyncChangeCursorPosition, _ *domain.SyncDeviceState) {
			actor.UserID = 0
		}, domain.ErrInvalidSessionData},
		{"missing device", func(actor *domain.Actor, _ *domain.SyncChangeCursorPosition, _ *domain.SyncDeviceState) {
			actor.DeviceID = 0
		}, domain.ErrInvalidSessionData},
		{"missing session", func(actor *domain.Actor, _ *domain.SyncChangeCursorPosition, _ *domain.SyncDeviceState) {
			actor.SessionID = 0
		}, domain.ErrInvalidSessionData},
		{"wrong cursor account", func(_ *domain.Actor, position *domain.SyncChangeCursorPosition, _ *domain.SyncDeviceState) {
			position.UserID++
		}, domain.ErrInvalidSyncChangeCursor},
		{"wrong cursor device", func(_ *domain.Actor, position *domain.SyncChangeCursorPosition, _ *domain.SyncDeviceState) {
			position.DeviceID++
		}, domain.ErrInvalidSyncChangeCursor},
		{"zero cursor generation", func(_ *domain.Actor, position *domain.SyncChangeCursorPosition, _ *domain.SyncDeviceState) {
			position.Generation = domain.SyncCursorGeneration{}
		}, domain.ErrInvalidSyncChangeCursor},
		{"old generation", func(_ *domain.Actor, position *domain.SyncChangeCursorPosition, _ *domain.SyncDeviceState) {
			position.Generation[15] ^= 1
		}, domain.ErrSyncSnapshotExpired},
		{"device awaiting baseline", func(_ *domain.Actor, _ *domain.SyncChangeCursorPosition, state *domain.SyncDeviceState) {
			state.Status, state.AckSequence = domain.SyncDeviceStatusResetRequired, 0
		}, domain.ErrSyncSnapshotExpired},
		{"pending with nonzero position", func(_ *domain.Actor, _ *domain.SyncChangeCursorPosition, state *domain.SyncDeviceState) {
			state.Status = domain.SyncDeviceStatusResetRequired
		}, domain.ErrInvalidSyncDeviceData},
		{"unknown status", func(_ *domain.Actor, _ *domain.SyncChangeCursorPosition, state *domain.SyncDeviceState) {
			state.Status = "UNKNOWN"
		}, domain.ErrInvalidSyncDeviceData},
		{"zero stored generation", func(_ *domain.Actor, _ *domain.SyncChangeCursorPosition, state *domain.SyncDeviceState) {
			state.Generation = domain.SyncCursorGeneration{}
		}, domain.ErrInvalidSyncDeviceData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			identity := actor
			position := domain.SyncChangeCursorPosition{UserID: actor.UserID, DeviceID: actor.DeviceID, Generation: generation, Sequence: 19}
			state := domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: generation, AckSequence: 17}
			scenario.configure(&identity, &position, &state)

			// 2. 拒绝异常身份或设备，返回可区分的领域错误。
			decision, err := ValidateSyncChangeConfirmation(identity, position, state, bounds)
			if decision != (SyncChangeConfirmationDecision{}) || !errors.Is(err, scenario.wantError) {
				t.Fatalf("异常确认仍交付 %+v，错误 %v，预期 %v", decision, err, scenario.wantError)
			}
		})
	}
}
