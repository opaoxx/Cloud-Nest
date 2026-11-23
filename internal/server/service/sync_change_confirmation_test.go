package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSyncChangeConfirmationServicePreflight(t *testing.T) {
	// 1. 生成有效增量游标与快照末页凭据，用真实验签器检查事务前拒绝。
	baselineVerifier, binding, expiresAt, masterKey := syncSnapshotBaselineVerificationFixture(t)
	baselineToken, err := baselineVerifier.signer.Sign(binding, 17, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewSyncChangeCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: binding.UserID, DeviceID: binding.DeviceID, SessionID: 27}
	cursor, err := verifier.signer.Sign(domain.SyncChangeCursorPosition{
		UserID: actor.UserID, DeviceID: actor.DeviceID, Generation: binding.Generation, Sequence: 19,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherAccount, otherDevice := actor, actor
	otherAccount.UserID, otherDevice.DeviceID = 7, 29
	for _, scenario := range []struct {
		name, cursor string
		identity     domain.Actor
		canceled     bool
		wantError    error
	}{
		{"missing cursor", "", actor, false, domain.ErrInvalidSyncChangeCursor},
		{"tampered signature", cursor + "A", actor, false, domain.ErrInvalidSyncChangeCursor},
		{"wrong token purpose", baselineToken, actor, false, domain.ErrInvalidSyncChangeCursor},
		{"different account", cursor, otherAccount, false, domain.ErrInvalidSyncChangeCursor},
		{"different device", cursor, otherDevice, false, domain.ErrInvalidSyncChangeCursor},
		{"missing session", cursor, domain.Actor{UserID: actor.UserID, DeviceID: actor.DeviceID}, false, domain.ErrInvalidSessionData},
		{"request already canceled", cursor, actor, true, context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario.canceled {
				cancel()
			}
			confirmer := NewSyncChangeConfirmationService(verifier,
				func(context.Context, domain.Actor, func(context.Context, SyncChangeConfirmationTransaction) error) error {
					t.Fatal("已拒绝的增量确认进入了账号事务")
					return nil
				})
			if ack, err := confirmer.Confirm(ctx, scenario.identity, scenario.cursor); ack != 0 || !errors.Is(err, scenario.wantError) {
				t.Fatalf("事务前拒绝不符：确认位置 %d，错误 %v，预期 %v", ack, err, scenario.wantError)
			}
		})
	}
}

func TestSyncChangeConfirmationServiceTransactionOutcome(t *testing.T) {
	// 1. 模拟外层事务的提交、回滚、失败和取消，零序号也作为正常结果验证。
	_, binding, _, masterKey := syncSnapshotBaselineVerificationFixture(t)
	verifier, err := NewSyncChangeCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: binding.UserID, DeviceID: binding.DeviceID, SessionID: 27}
	rollbackError := errors.New("fixture rollback")
	commitError := fmt.Errorf("fixture commit: %w", domain.ErrUserTransactionCommitUnconfirmed)
	for _, scenario := range []struct {
		name                          string
		requested, current, highWater uint64
		beforeAction, afterAction     error
		skipAction, cancelRequest     bool
		wantCalls                     string
		wantPosition                  uint64
		wantError                     error
	}{
		{name: "commit zero position", wantCalls: "state bounds record"},
		{name: "commit advances position", requested: 19, current: 17, highWater: 22, wantCalls: "state bounds record", wantPosition: 19},
		{name: "commit returns current position for older cursor", requested: 19, current: 22, highWater: 22, wantCalls: "state bounds", wantPosition: 22},
		{name: "callback failure", requested: 23, current: 17, highWater: 22, wantCalls: "state bounds", wantError: domain.ErrInvalidSyncChangeCursor},
		{name: "session revoked before callback", requested: 19, current: 17, highWater: 22,
			beforeAction: domain.ErrSessionRevoked, wantError: domain.ErrSessionRevoked},
		{name: "rollback after callback", requested: 19, current: 17, highWater: 22,
			afterAction: rollbackError, wantCalls: "state bounds record", wantError: rollbackError},
		{name: "commit result unknown", requested: 19, current: 17, highWater: 22,
			afterAction: commitError, wantCalls: "state bounds record", wantError: domain.ErrUserTransactionCommitUnconfirmed},
		{name: "request canceled before delivery", requested: 19, current: 17, highWater: 22,
			cancelRequest: true, wantCalls: "state bounds record", wantError: context.Canceled},
		{name: "transaction omitted callback", requested: 19, current: 17, highWater: 22, skipAction: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cursor, err := verifier.signer.Sign(domain.SyncChangeCursorPosition{
				UserID: actor.UserID, DeviceID: actor.DeviceID, Generation: binding.Generation, Sequence: scenario.requested,
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture := &syncChangeConfirmationTransactionFixture{
				state:   domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: binding.Generation, AckSequence: scenario.current},
				bounds:  domain.SyncChangeSequenceBounds{HighWaterSequence: scenario.highWater},
				matched: true,
			}
			transactions := 0
			confirmer := NewSyncChangeConfirmationService(verifier,
				func(requestContext context.Context, received domain.Actor, action func(context.Context, SyncChangeConfirmationTransaction) error) error {
					transactions++
					if requestContext != ctx || received != actor {
						t.Fatal("事务未获得原请求上下文及身份")
					}
					if scenario.beforeAction != nil || scenario.skipAction {
						return scenario.beforeAction
					}
					txContext, end := context.WithCancel(requestContext)
					defer end()
					if err := action(txContext, fixture); err != nil {
						return fmt.Errorf("fixture callback: %w", err)
					}
					if scenario.cancelRequest {
						cancel()
					}
					return scenario.afterAction
				})

			// 2. 只有事务完成才交付位置，并核对错误返回与实际调用。
			ack, err := confirmer.Confirm(ctx, actor, cursor)
			if transactions != 1 || strings.Join(fixture.calls, " ") != scenario.wantCalls {
				t.Fatalf("事务调用次数或顺序不符：%d，%v", transactions, fixture.calls)
			}
			if scenario.skipAction {
				if ack != 0 || err == nil {
					t.Fatalf("跳过事务回调仍交付位置：%d，错误 %v", ack, err)
				}
			} else if ack != scenario.wantPosition || !errors.Is(err, scenario.wantError) {
				t.Fatalf("确认位置 %d，错误 %v，预期 %d 和 %v", ack, err, scenario.wantPosition, scenario.wantError)
			}
		})
	}
}
