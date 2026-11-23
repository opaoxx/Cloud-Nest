package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSyncSnapshotBaselineConfirmationServicePreflight(t *testing.T) {
	// 1. 使用真实签发器准备凭据，验证拒绝发生在开启账号事务前。
	verifier, binding, expiresAt, masterKey := syncSnapshotBaselineVerificationFixture(t)
	token, err := verifier.signer.Sign(binding, 17, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	cursorSigner, err := NewSyncSnapshotCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	pageCursor, err := cursorSigner.Sign(binding, binding.PageSize, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: binding.UserID, DeviceID: binding.DeviceID, SessionID: 3}
	otherDevice := actor
	otherDevice.DeviceID--
	otherSnapshotID := binding.SnapshotID
	otherSnapshotID[15] ^= 1
	beforeExpiry := expiresAt.Add(-time.Nanosecond)
	for _, testCase := range []struct {
		name, token string
		actor       domain.Actor
		snapshotID  uuid.UUID
		now         time.Time
		want        error
	}{
		{"tampered signature", token + "A", actor, binding.SnapshotID, beforeExpiry, domain.ErrInvalidSyncSnapshotBaselineToken},
		{"pagination token", pageCursor, actor, binding.SnapshotID, beforeExpiry, domain.ErrInvalidSyncSnapshotBaselineToken},
		{"wrong device", token, otherDevice, binding.SnapshotID, beforeExpiry, domain.ErrInvalidSyncSnapshotBaselineToken},
		{"wrong path", token, actor, otherSnapshotID, beforeExpiry, domain.ErrInvalidSyncSnapshotBaselineToken},
		{"already expired", token, actor, binding.SnapshotID, expiresAt, domain.ErrSyncSnapshotExpired},
		{"incomplete actor", token, domain.Actor{}, binding.SnapshotID, beforeExpiry, domain.ErrInvalidSessionData},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			confirmer := NewSyncSnapshotBaselineConfirmationService(verifier,
				func(context.Context, domain.Actor, func(context.Context, SyncSnapshotBaselineConfirmationTransaction) error) error {
					t.Fatal("已拒绝的请求进入了事务")
					return nil
				}, func() time.Time { return testCase.now })
			if position, err := confirmer.Confirm(t.Context(), testCase.actor, testCase.snapshotID, testCase.token); position != 0 || !errors.Is(err, testCase.want) {
				t.Fatalf("确认位置 %d，错误 %v，预期 %v", position, err, testCase.want)
			}
		})
	}
}

func TestSyncSnapshotBaselineConfirmationServiceTransactionOutcome(t *testing.T) {
	// 1. 模拟提交回执、回滚和进入账号锁后的时钟变化。
	verifier, binding, expiresAt, _ := syncSnapshotBaselineVerificationFixture(t)
	actor := domain.Actor{UserID: binding.UserID, DeviceID: binding.DeviceID, SessionID: 3}
	rolledBack := errors.New("synthetic rollback")
	commitUnconfirmed := fmt.Errorf("synthetic commit error: %w", domain.ErrUserTransactionCommitUnconfirmed)
	for _, testCase := range []struct {
		name                             string
		cutoff, want                     uint64
		beforeActionErr, afterActionErr  error
		wantErr                          error
		expireInLock, skipAction, cancel bool
		calls                            []string
	}{
		{name: "zero is a successful position", calls: []string{"read", "validate", "activate"}},
		{name: "commit returns a nonzero position", cutoff: 17, want: 17, calls: []string{"read", "validate", "activate"}},
		{name: "rollback after activation", cutoff: 17, afterActionErr: rolledBack, wantErr: rolledBack, calls: []string{"read", "validate", "activate"}},
		{name: "unconfirmed commit", cutoff: 17, afterActionErr: commitUnconfirmed, wantErr: domain.ErrUserTransactionCommitUnconfirmed,
			calls: []string{"read", "validate", "activate"}},
		{name: "revoked session", cutoff: 17, beforeActionErr: domain.ErrSessionRevoked, wantErr: domain.ErrSessionRevoked},
		{name: "expired while waiting for lock", cutoff: 17, expireInLock: true, wantErr: domain.ErrSyncSnapshotExpired},
		{name: "transaction omitted callback", cutoff: 17, skipAction: true},
		{name: "canceled before delivery", cutoff: 17, cancel: true, wantErr: context.Canceled, calls: []string{"read", "validate", "activate"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			token, err := verifier.signer.Sign(binding, testCase.cutoff, expiresAt)
			if err != nil {
				t.Fatal(err)
			}
			stamp := expiresAt.Add(-time.Nanosecond)
			fixture := &baselineConfirmationFixture{
				state:   domain.SyncDeviceState{Status: domain.SyncDeviceStatusResetRequired, Generation: binding.Generation},
				current: testCase.cutoff, updateMatched: true,
			}
			transactions := 0
			confirmer := NewSyncSnapshotBaselineConfirmationService(verifier,
				func(parent context.Context, received domain.Actor, action func(context.Context, SyncSnapshotBaselineConfirmationTransaction) error) error {
					transactions++
					if parent != ctx || received != actor {
						t.Fatal("事务上下文或身份不一致")
					}
					if testCase.beforeActionErr != nil || testCase.skipAction {
						return testCase.beforeActionErr
					}
					if testCase.expireInLock {
						stamp = expiresAt
					}
					if err := action(parent, fixture); err != nil {
						return fmt.Errorf("synthetic callback failure: %w", err)
					}
					if testCase.cancel {
						cancel()
					}
					return testCase.afterActionErr
				}, func() time.Time { return stamp })

			// 2. 只交付成功提交的位置，保留调用方可识别的失败原因。
			position, err := confirmer.Confirm(ctx, actor, binding.SnapshotID, token)
			if testCase.skipAction {
				if err == nil || position != 0 {
					t.Fatal("未运行事务回调却返回了成功结果")
				}
			} else if position != testCase.want || !errors.Is(err, testCase.wantErr) {
				t.Fatalf("确认位置 %d，错误 %v，预期 %d 和 %v", position, err, testCase.want, testCase.wantErr)
			}
			if transactions != 1 || !slices.Equal(fixture.calls, testCase.calls) {
				t.Fatalf("确认尝试次数或事务操作不符：%d, %v", transactions, fixture.calls)
			}
		})
	}
}
