package service

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestValidateSyncChangePullStart(t *testing.T) {
	// 1. 模拟同一读视图内的设备状态、时间与日志上下界。
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 27}
	generation := domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe}
	expiresAt := time.Date(2026, 11, 23, 2, 10, 0, 123456789, time.UTC)
	justBeforeExpiry := expiresAt.Add(-time.Nanosecond)
	window := domain.SyncChangeSequenceBounds{MinimumPosition: 17, HighWaterSequence: 22}
	active := func(ack uint64) domain.SyncDeviceState {
		return domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: generation, AckSequence: ack}
	}
	cursor := func(sequence uint64) SyncChangePullStart {
		return SyncChangePullStart{Position: domain.SyncChangeCursorPosition{
			UserID: actor.UserID, DeviceID: actor.DeviceID, Generation: generation, Sequence: sequence,
		}}
	}
	baseline := func(sequence uint64) SyncChangePullStart {
		start := cursor(sequence)
		start.BaselineExpiresAt = expiresAt
		return start
	}
	oldGeneration := cursor(17)
	oldGeneration.Position.Generation[15] ^= 1
	pending := domain.SyncDeviceState{Status: domain.SyncDeviceStatusResetRequired, Generation: generation}
	badPending := pending
	badPending.AckSequence = 17
	badGeneration := active(17)
	badGeneration.Generation = domain.SyncCursorGeneration{}
	badStatus := active(17)
	badStatus.Status = "UNKNOWN"
	for _, testCase := range []struct {
		name   string
		start  SyncChangePullStart
		state  domain.SyncDeviceState
		bounds domain.SyncChangeSequenceBounds
		now    time.Time
		want   error
	}{
		{name: "minimum cursor", start: cursor(17), state: active(17), bounds: window},
		{name: "high water cursor", start: cursor(22), state: active(17), bounds: window},
		{name: "cursor ahead of acknowledgment", start: cursor(20), state: active(17), bounds: window},
		{name: "cursor replay older than acknowledgment", start: cursor(17), state: active(20), bounds: window},
		{name: "confirmed baseline", start: baseline(17), state: active(17), bounds: window, now: justBeforeExpiry},
		{name: "baseline uses signed position after advancement", start: baseline(17), state: active(20), bounds: window, now: justBeforeExpiry},
		{name: "empty account and zero baseline", start: baseline(0), state: active(0), now: justBeforeExpiry},
		{name: "no retained events at high water", start: cursor(22), state: active(22), bounds: domain.SyncChangeSequenceBounds{MinimumPosition: 22, HighWaterSequence: 22}},
		{name: "maximum unsigned sequence", start: cursor(math.MaxUint64), state: active(math.MaxUint64),
			bounds: domain.SyncChangeSequenceBounds{MinimumPosition: math.MaxUint64 - 1, HighWaterSequence: math.MaxUint64}},
		{name: "baseline expires during snapshot setup", start: baseline(17), state: active(17), bounds: window, now: expiresAt, want: domain.ErrSyncSnapshotExpired},
		{name: "pending baseline", start: baseline(17), state: pending, bounds: window, now: justBeforeExpiry, want: domain.ErrSyncSnapshotExpired},
		{name: "pending device with old cursor", start: cursor(17), state: pending, bounds: window, want: domain.ErrSyncSnapshotExpired},
		{name: "different final generation byte", start: oldGeneration, state: active(17), bounds: window, want: domain.ErrSyncSnapshotExpired},
		{name: "position pruned", start: cursor(16), state: active(17), bounds: window, want: domain.ErrSyncSnapshotExpired},
		{name: "cursor ahead of high water", start: cursor(23), state: active(17), bounds: window, want: domain.ErrInvalidSyncChangeCursor},
		{name: "baseline ahead of high water", start: baseline(23), state: active(17), bounds: window, now: justBeforeExpiry, want: domain.ErrInvalidSyncSnapshotBaselineToken},
		{name: "baseline acknowledgment contradicts cutoff", start: baseline(19), state: active(17), bounds: window, now: justBeforeExpiry, want: domain.ErrInvalidSyncDeviceData},
		{name: "acknowledgment exceeds high water", start: cursor(17), state: active(23), bounds: window, want: domain.ErrInvalidSyncDeviceData},
		{name: "nonzero pending acknowledgment", start: cursor(17), state: badPending, bounds: window, want: domain.ErrInvalidSyncDeviceData},
		{name: "zero device generation", start: cursor(17), state: badGeneration, bounds: window, want: domain.ErrInvalidSyncDeviceData},
		{name: "unknown device status", start: cursor(17), state: badStatus, bounds: window, want: domain.ErrInvalidSyncDeviceData},
		{name: "inverted bounds", start: cursor(17), state: active(17),
			bounds: domain.SyncChangeSequenceBounds{MinimumPosition: 23, HighWaterSequence: 22}, want: domain.ErrInvalidSyncSnapshotSequence},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 2. 验证复核结果和原始起点，尤其是设备确认推进后的首次拉取。
			start := testCase.start
			err := ValidateSyncChangePullStart(actor, &start, testCase.state, testCase.bounds, testCase.now)
			if !errors.Is(err, testCase.want) || start != testCase.start {
				t.Fatalf("复核错误 %v，预期 %v，起点 %+v", err, testCase.want, start)
			}
		})
	}
}

func TestValidateSyncChangePullStartRejectsInvalidIdentity(t *testing.T) {
	// 1. 账号或设备不符时保留原凭据的错误类型，缺失身份或起点时提前停止。
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 27}
	generation := domain.SyncCursorGeneration{15: 1}
	state := domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: generation}
	var bounds domain.SyncChangeSequenceBounds
	start := SyncChangePullStart{Position: domain.SyncChangeCursorPosition{UserID: 8, DeviceID: 19, Generation: generation}}
	if err := ValidateSyncChangePullStart(actor, &start, state, bounds, time.Time{}); !errors.Is(err, domain.ErrInvalidSyncChangeCursor) {
		t.Fatalf("游标账号不匹配：%v", err)
	}
	start.Position.UserID, start.Position.DeviceID = 7, 20
	start.BaselineExpiresAt = time.Date(2026, 11, 23, 2, 10, 0, 0, time.UTC)
	if err := ValidateSyncChangePullStart(actor, &start, state, bounds, start.BaselineExpiresAt); !errors.Is(err, domain.ErrInvalidSyncSnapshotBaselineToken) {
		t.Fatalf("末页凭据设备不匹配：%v", err)
	}
	missingSession := actor
	missingSession.SessionID = 0
	if err := ValidateSyncChangePullStart(missingSession, &start, state, bounds, time.Time{}); !errors.Is(err, domain.ErrInvalidSessionData) {
		t.Fatalf("缺少会话仍通过复核：%v", err)
	}
	if err := ValidateSyncChangePullStart(actor, nil, state, bounds, time.Time{}); err == nil {
		t.Fatal("缺少起点仍通过复核")
	}
}
