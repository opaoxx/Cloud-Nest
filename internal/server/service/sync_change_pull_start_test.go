package service

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSyncChangePullStartResolverPreservesPosition(t *testing.T) {
	// 1. 用同一设备代次签发两种真实凭据，验证两条分支交付相同的位置。
	baselineVerifier, binding, expiresAt, masterKey := syncSnapshotBaselineVerificationFixture(t)
	cursorVerifier, err := NewSyncChangeCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: binding.UserID, DeviceID: binding.DeviceID, SessionID: 7}
	clockCalls := 0
	resolver := NewSyncChangePullStartResolver(baselineVerifier, cursorVerifier, func() time.Time {
		clockCalls++
		return expiresAt.Add(-time.Nanosecond)
	})
	for _, sequence := range []uint64{0, 17, math.MaxUint64} {
		position := domain.SyncChangeCursorPosition{UserID: actor.UserID, DeviceID: actor.DeviceID, Generation: binding.Generation, Sequence: sequence}
		baselineToken, err := baselineVerifier.signer.Sign(binding, sequence, expiresAt)
		if err != nil {
			t.Fatal(err)
		}
		cursor, err := cursorVerifier.signer.Sign(position)
		if err != nil {
			t.Fatal(err)
		}

		// 2. 首次拉取保留末页原期限。后续拉取允许同设备换会话且不读取时钟。
		priorClockCalls := clockCalls
		first, err := resolver.Resolve(actor, baselineToken, "")
		if err != nil || first == nil || first.Position != position || !first.BaselineExpiresAt.Equal(expiresAt) || clockCalls != priorClockCalls+1 {
			t.Fatalf("首次起点未保留位置或期限：%+v，错误 %v", first, err)
		}
		laterSession := actor
		laterSession.SessionID++
		next, err := resolver.Resolve(laterSession, "", cursor)
		if err != nil || next == nil || next.Position != position || !next.BaselineExpiresAt.IsZero() || clockCalls != priorClockCalls+1 {
			t.Fatalf("续读起点或时钟行为不符：%+v，错误 %v", next, err)
		}
	}
}

func TestSyncChangePullStartResolverRejectsInvalidCredentials(t *testing.T) {
	// 1. 准备两种独立用途的凭据，覆盖组合、身份、签名与到期边界。
	baselineVerifier, binding, expiresAt, masterKey := syncSnapshotBaselineVerificationFixture(t)
	cursorVerifier, err := NewSyncChangeCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	baselineToken, err := baselineVerifier.signer.Sign(binding, 17, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := cursorVerifier.signer.Sign(domain.SyncChangeCursorPosition{
		UserID: binding.UserID, DeviceID: binding.DeviceID, Generation: binding.Generation, Sequence: 17,
	})
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: binding.UserID, DeviceID: binding.DeviceID, SessionID: 7}
	otherDevice := actor
	otherDevice.DeviceID--
	for _, testCase := range []struct {
		name       string
		actor      domain.Actor
		baseline   string
		cursor     string
		clockCalls int
		now        time.Time
		wantErr    error
	}{
		{name: "missing both", actor: actor},
		{name: "both valid credentials", actor: actor, baseline: baselineToken, cursor: cursor},
		{name: "invalid baseline cannot fall back to cursor", actor: actor, baseline: "a.b", cursor: cursor},
		{name: "missing session", actor: domain.Actor{UserID: actor.UserID, DeviceID: actor.DeviceID}, cursor: cursor, wantErr: domain.ErrInvalidSessionData},
		{name: "cursor in baseline field", actor: actor, baseline: cursor, clockCalls: 1, wantErr: domain.ErrInvalidSyncSnapshotBaselineToken},
		{name: "baseline in cursor field", actor: actor, cursor: baselineToken, wantErr: domain.ErrInvalidSyncChangeCursor},
		{name: "expired baseline", actor: actor, baseline: baselineToken, clockCalls: 1, now: expiresAt, wantErr: domain.ErrSyncSnapshotExpired},
		{name: "tampered baseline", actor: actor, baseline: baselineToken + "A", clockCalls: 1, wantErr: domain.ErrInvalidSyncSnapshotBaselineToken},
		{name: "tampered cursor", actor: actor, cursor: cursor + "A", wantErr: domain.ErrInvalidSyncChangeCursor},
		{name: "baseline for another device", actor: otherDevice, baseline: baselineToken, clockCalls: 1, wantErr: domain.ErrInvalidSyncSnapshotBaselineToken},
		{name: "cursor for another device", actor: otherDevice, cursor: cursor, wantErr: domain.ErrInvalidSyncChangeCursor},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 2. 失败交付空结果，并保留原签名或期限错误。
			now := testCase.now
			if now.IsZero() {
				now = expiresAt.Add(-time.Nanosecond)
			}
			clockCalls := 0
			resolver := NewSyncChangePullStartResolver(baselineVerifier, cursorVerifier, func() time.Time {
				clockCalls++
				return now
			})
			start, err := resolver.Resolve(testCase.actor, testCase.baseline, testCase.cursor)
			if start != nil || err == nil || (testCase.wantErr != nil && !errors.Is(err, testCase.wantErr)) || clockCalls != testCase.clockCalls {
				t.Fatalf("失败交付不符：起点 %+v，错误 %v，时钟调用 %d", start, err, clockCalls)
			}
		})
	}
}
