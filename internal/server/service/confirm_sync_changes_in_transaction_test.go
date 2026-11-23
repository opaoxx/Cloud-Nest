package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type syncChangeConfirmationTransactionFixture struct {
	state                         domain.SyncDeviceState
	bounds                        domain.SyncChangeSequenceBounds
	stateError, boundsError       error
	recordError                   error
	matched                       bool
	calls                         []string
	recordedGeneration            domain.SyncCursorGeneration
	recordedPrevious, recordedAck uint64
	cancel                        context.CancelFunc
	cancelAfter                   string
}

func (fixture *syncChangeConfirmationTransactionFixture) FindCurrentSyncDeviceState(context.Context) (domain.SyncDeviceState, error) {
	fixture.calls = append(fixture.calls, "state")
	return fixture.state, fixture.stateError
}

func (fixture *syncChangeConfirmationTransactionFixture) ReadCurrentSyncChangeSequenceBounds(context.Context) (domain.SyncChangeSequenceBounds, error) {
	fixture.calls = append(fixture.calls, "bounds")
	if fixture.cancelAfter == "bounds" {
		fixture.cancel()
	}
	return fixture.bounds, fixture.boundsError
}

func (fixture *syncChangeConfirmationTransactionFixture) RecordSyncChangeConfirmation(
	_ context.Context, generation domain.SyncCursorGeneration, expectedSequence, confirmedSequence uint64,
) (bool, error) {
	fixture.calls = append(fixture.calls, "record")
	fixture.recordedGeneration = generation
	fixture.recordedPrevious, fixture.recordedAck = expectedSequence, confirmedSequence
	if fixture.cancelAfter == "record" {
		fixture.cancel()
	}
	return fixture.matched, fixture.recordError
}

func TestConfirmSyncChangesInTransaction(t *testing.T) {
	// 1. 准备同一账号的已验签位置、设备状态和日志区间。
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 27}
	generation := domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe}
	fixtureError := errors.New("fixture persistence failure")
	for _, scenario := range []struct {
		name          string
		position, ack uint64
		change        func(*syncChangeConfirmationTransactionFixture, *domain.SyncChangeCursorPosition)
		wantCalls     string
		wantAck       uint64
		wantError     error
	}{
		{name: "advance", position: 19, ack: 17, wantCalls: "state bounds record", wantAck: 19},
		{name: "equal records activity", position: 19, ack: 19, wantCalls: "state bounds record", wantAck: 19},
		{name: "older returns committed position", position: 19, ack: 22, wantCalls: "state bounds", wantAck: 22},
		{name: "invalid identity before any read", position: 19, ack: 17, wantError: domain.ErrInvalidSyncChangeCursor,
			change: func(_ *syncChangeConfirmationTransactionFixture, position *domain.SyncChangeCursorPosition) {
				position.DeviceID++
			}},
		{name: "missing device before reading bounds", position: 19, ack: 17, wantCalls: "state", wantError: domain.ErrSyncSnapshotExpired,
			change: func(fixture *syncChangeConfirmationTransactionFixture, _ *domain.SyncChangeCursorPosition) {
				fixture.stateError = domain.ErrSyncDeviceNotFound
			}},
		{name: "state read fails", position: 19, ack: 17, wantCalls: "state", wantError: fixtureError,
			change: func(fixture *syncChangeConfirmationTransactionFixture, _ *domain.SyncChangeCursorPosition) {
				fixture.stateError = fixtureError
			}},
		{name: "old generation before reading bounds", position: 19, ack: 17, wantCalls: "state", wantError: domain.ErrSyncSnapshotExpired,
			change: func(_ *syncChangeConfirmationTransactionFixture, position *domain.SyncChangeCursorPosition) {
				position.Generation[15] ^= 1
			}},
		{name: "pending device before reading bounds", position: 19, wantCalls: "state", wantError: domain.ErrSyncSnapshotExpired,
			change: func(fixture *syncChangeConfirmationTransactionFixture, _ *domain.SyncChangeCursorPosition) {
				fixture.state.Status = domain.SyncDeviceStatusResetRequired
			}},
		{name: "bounds read fails", position: 19, ack: 17, wantCalls: "state bounds", wantError: fixtureError,
			change: func(fixture *syncChangeConfirmationTransactionFixture, _ *domain.SyncChangeCursorPosition) {
				fixture.boundsError = fixtureError
			}},
		{name: "older position after pruning", position: 16, ack: 22, wantCalls: "state bounds", wantError: domain.ErrSyncSnapshotExpired},
		{name: "above high water", position: 23, ack: 22, wantCalls: "state bounds", wantError: domain.ErrInvalidSyncChangeCursor},
		{name: "update condition not matched", position: 19, ack: 17, wantCalls: "state bounds record", wantError: domain.ErrInvalidSyncDeviceData,
			change: func(fixture *syncChangeConfirmationTransactionFixture, _ *domain.SyncChangeCursorPosition) {
				fixture.matched = false
			}},
		{name: "write fails", position: 19, ack: 17, wantCalls: "state bounds record", wantError: fixtureError,
			change: func(fixture *syncChangeConfirmationTransactionFixture, _ *domain.SyncChangeCursorPosition) {
				fixture.recordError = fixtureError
			}},
		{name: "canceled before record", position: 19, ack: 17, wantCalls: "state bounds", wantError: context.Canceled,
			change: func(fixture *syncChangeConfirmationTransactionFixture, _ *domain.SyncChangeCursorPosition) {
				fixture.cancelAfter = "bounds"
			}},
		{name: "canceled after record", position: 19, ack: 17, wantCalls: "state bounds record", wantError: context.Canceled,
			change: func(fixture *syncChangeConfirmationTransactionFixture, _ *domain.SyncChangeCursorPosition) {
				fixture.cancelAfter = "record"
			}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			position := domain.SyncChangeCursorPosition{UserID: actor.UserID, DeviceID: actor.DeviceID, Generation: generation, Sequence: scenario.position}
			fixture := &syncChangeConfirmationTransactionFixture{
				state:   domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: generation, AckSequence: scenario.ack},
				bounds:  domain.SyncChangeSequenceBounds{MinimumPosition: 17, HighWaterSequence: 22},
				matched: true, cancel: cancel,
			}
			if scenario.change != nil {
				scenario.change(fixture, &position)
			}

			// 2. 验证短路顺序、更新参数与失败时的空结果。
			ack, err := ConfirmSyncChangesInTransaction(ctx, actor, &position, fixture)
			if ack != scenario.wantAck || !errors.Is(err, scenario.wantError) || strings.Join(fixture.calls, " ") != scenario.wantCalls {
				t.Fatalf("确认位置 %d，错误 %v，调用 %v，预期 %d、%v、%s", ack, err, fixture.calls, scenario.wantAck, scenario.wantError, scenario.wantCalls)
			}
			if strings.HasSuffix(scenario.wantCalls, "record") &&
				(fixture.recordedGeneration != generation || fixture.recordedPrevious != scenario.ack || fixture.recordedAck != scenario.position) {
				t.Fatal("条件更新未收到复核过的完整代次和位置")
			}
		})
	}
}
