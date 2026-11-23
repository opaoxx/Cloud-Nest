package service

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type baselineConfirmationFixture struct {
	state              domain.SyncDeviceState
	minimum, current   uint64
	readErr, updateErr error
	updateMatched      bool
	checked            []uint64
	calls              []string
	updatedGeneration  domain.SyncCursorGeneration
	updatedCutoff      uint64
	afterUpdate        func()
}

func (fixture *baselineConfirmationFixture) FindCurrentSyncDeviceState(context.Context) (domain.SyncDeviceState, error) {
	fixture.calls = append(fixture.calls, "read")
	return fixture.state, fixture.readErr
}

func (fixture *baselineConfirmationFixture) ValidateSyncSnapshotSequence(_ context.Context, sequence uint64) error {
	fixture.calls = append(fixture.calls, "validate")
	fixture.checked = append(fixture.checked, sequence)
	if sequence > fixture.current {
		return domain.ErrInvalidSyncSnapshotSequence
	}
	if sequence < fixture.minimum {
		return domain.ErrSyncSnapshotExpired
	}
	return nil
}

func (fixture *baselineConfirmationFixture) ActivateSyncDeviceBaseline(_ context.Context, generation domain.SyncCursorGeneration, cutoff uint64) (bool, error) {
	fixture.calls = append(fixture.calls, "activate")
	fixture.updatedGeneration, fixture.updatedCutoff = generation, cutoff
	if fixture.afterUpdate != nil {
		fixture.afterUpdate()
	}
	return fixture.updateMatched, fixture.updateErr
}

func TestConfirmSyncSnapshotBaselineInTransaction(t *testing.T) {
	// 1. 复用签发与验签夹具，锁内复核由独立时钟和仓储替身驱动。
	verifier, binding, expiresAt, _ := syncSnapshotBaselineVerificationFixture(t)
	actor := domain.Actor{UserID: binding.UserID, DeviceID: binding.DeviceID, SessionID: 3}
	beforeExpiry := expiresAt.Add(-time.Nanosecond)
	pending := domain.SyncDeviceState{Status: domain.SyncDeviceStatusResetRequired, Generation: binding.Generation}
	active := func(position uint64) domain.SyncDeviceState {
		return domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: binding.Generation, AckSequence: position}
	}
	oldGeneration := pending
	oldGeneration.Generation[15] ^= 1
	invalidPending := pending
	invalidPending.AckSequence = 1
	unknownState := pending
	unknownState.Status = "UNKNOWN"
	databaseError := errors.New("synthetic database failure")
	for _, testCase := range []struct {
		name                             string
		state                            domain.SyncDeviceState
		cutoff, minimum, current, want   uint64
		readErr, updateErr, wantErr      error
		startExpired, expireDuringRead   bool
		cancelAfterUpdate, updateMatched bool
		checks                           []uint64
		calls                            []string
	}{
		{name: "first confirmation at zero", state: pending, updateMatched: true, checks: []uint64{0}, calls: []string{"read", "validate", "activate"}},
		{name: "first confirmation at oldest readable position", state: pending, cutoff: 17, minimum: 17, current: 22, want: 17,
			updateMatched: true, checks: []uint64{17}, calls: []string{"read", "validate", "activate"}},
		{name: "first confirmation at maximum sequence", state: pending, cutoff: math.MaxUint64, minimum: math.MaxUint64, current: math.MaxUint64,
			want: math.MaxUint64, updateMatched: true, checks: []uint64{math.MaxUint64}, calls: []string{"read", "validate", "activate"}},
		{name: "retry at original position", state: active(17), cutoff: 17, minimum: 17, current: 22, want: 17,
			checks: []uint64{17}, calls: []string{"read", "validate"}},
		{name: "retry with later position", state: active(20), cutoff: 17, minimum: 17, current: 22, want: 20,
			checks: []uint64{17, 20}, calls: []string{"read", "validate", "validate"}},
		{name: "active position ahead of account", state: active(23), cutoff: 17, minimum: 17, current: 22,
			wantErr: domain.ErrInvalidSyncSnapshotSequence, checks: []uint64{17, 23}, calls: []string{"read", "validate", "validate"}},
		{name: "active position behind cutoff", state: active(16), cutoff: 17, minimum: 17, current: 22,
			wantErr: domain.ErrInvalidSyncDeviceData, checks: []uint64{17}, calls: []string{"read", "validate"}},
		{name: "cutoff pruned even on retry", state: active(20), cutoff: 16, minimum: 17, current: 22,
			wantErr: domain.ErrSyncSnapshotExpired, checks: []uint64{16}, calls: []string{"read", "validate"}},
		{name: "cutoff ahead of account", state: pending, cutoff: 23, minimum: 17, current: 22,
			wantErr: domain.ErrInvalidSyncSnapshotSequence, checks: []uint64{23}, calls: []string{"read", "validate"}},
		{name: "already expired at lock acquisition", state: pending, startExpired: true, wantErr: domain.ErrSyncSnapshotExpired},
		{name: "expires during database review", state: pending, cutoff: 17, minimum: 17, current: 22, expireDuringRead: true,
			wantErr: domain.ErrSyncSnapshotExpired, checks: []uint64{17}, calls: []string{"read", "validate"}},
		{name: "active retry expires during database review", state: active(17), cutoff: 17, minimum: 17, current: 22, expireDuringRead: true,
			wantErr: domain.ErrSyncSnapshotExpired, checks: []uint64{17}, calls: []string{"read", "validate"}},
		{name: "different final generation byte", state: oldGeneration, wantErr: domain.ErrSyncSnapshotExpired, calls: []string{"read"}},
		{name: "missing sync device", readErr: domain.ErrSyncDeviceNotFound, wantErr: domain.ErrSyncSnapshotExpired, calls: []string{"read"}},
		{name: "read failure", readErr: databaseError, wantErr: databaseError, calls: []string{"read"}},
		{name: "pending with nonzero position", state: invalidPending, wantErr: domain.ErrInvalidSyncDeviceData,
			checks: []uint64{0}, calls: []string{"read", "validate"}},
		{name: "unknown device status", state: unknownState, wantErr: domain.ErrInvalidSyncDeviceData,
			checks: []uint64{0}, calls: []string{"read", "validate"}},
		{name: "conditional write cannot match locked row", state: pending, wantErr: domain.ErrInvalidSyncDeviceData,
			checks: []uint64{0}, calls: []string{"read", "validate", "activate"}},
		{name: "conditional write fails", state: pending, updateErr: databaseError, wantErr: databaseError,
			checks: []uint64{0}, calls: []string{"read", "validate", "activate"}},
		{name: "cancel after write", state: pending, cutoff: 17, minimum: 17, current: 22, cancelAfterUpdate: true, updateMatched: true,
			wantErr: context.Canceled, checks: []uint64{17}, calls: []string{"read", "validate", "activate"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 2. 验证原期限与锁内操作顺序，只在首次确认提交计划中的写入。
			token, err := verifier.signer.Sign(binding, testCase.cutoff, expiresAt)
			if err != nil {
				t.Fatal(err)
			}
			verified, err := verifier.Verify(actor.UserID, actor.DeviceID, binding.SnapshotID, token, beforeExpiry)
			if err != nil {
				t.Fatal(err)
			}
			fixture := &baselineConfirmationFixture{
				state: testCase.state, minimum: testCase.minimum, current: testCase.current,
				readErr: testCase.readErr, updateErr: testCase.updateErr, updateMatched: testCase.updateMatched,
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if testCase.cancelAfterUpdate {
				fixture.afterUpdate = cancel
			}
			clock := func() time.Time {
				if testCase.startExpired || (testCase.expireDuringRead && len(fixture.checked) > 0) {
					return expiresAt
				}
				return beforeExpiry
			}
			position, err := ConfirmSyncSnapshotBaselineInTransaction(ctx, actor, verified, clock, fixture)
			if position != testCase.want || !errors.Is(err, testCase.wantErr) {
				t.Fatalf("确认位置 %d，错误 %v，预期 %d 和 %v", position, err, testCase.want, testCase.wantErr)
			}
			if !slices.Equal(fixture.calls, testCase.calls) || !slices.Equal(fixture.checked, testCase.checks) {
				t.Fatalf("调用 %v，复核序号 %v，预期 %v 和 %v", fixture.calls, fixture.checked, testCase.calls, testCase.checks)
			}
			if slices.Contains(testCase.calls, "activate") && (fixture.updatedGeneration != binding.Generation || fixture.updatedCutoff != testCase.cutoff) {
				t.Fatalf("条件更新未使用凭据的代次和截止序号：%x, %d", fixture.updatedGeneration, fixture.updatedCutoff)
			}
		})
	}
}

func TestConfirmSyncSnapshotBaselineInTransactionRejectsWrongDevice(t *testing.T) {
	// 1. 将正确签发的凭据交给同一账号的另一设备，确认仓储未被调用。
	verifier, binding, expiresAt, _ := syncSnapshotBaselineVerificationFixture(t)
	token, err := verifier.signer.Sign(binding, 17, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return expiresAt.Add(-time.Nanosecond) }
	verified, err := verifier.Verify(binding.UserID, binding.DeviceID, binding.SnapshotID, token, clock())
	if err != nil {
		t.Fatal(err)
	}
	fixture := &baselineConfirmationFixture{}
	otherDevice := domain.Actor{UserID: binding.UserID, DeviceID: binding.DeviceID - 1, SessionID: 3}
	position, err := ConfirmSyncSnapshotBaselineInTransaction(t.Context(), otherDevice, verified, clock, fixture)
	if position != 0 || !errors.Is(err, domain.ErrInvalidSyncSnapshotBaselineToken) || len(fixture.calls) != 0 {
		t.Fatalf("不同设备取得了确认结果：%d，错误 %v，调用 %v", position, err, fixture.calls)
	}
}
