package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSyncChangePullService(t *testing.T) {
	// 1. 准备真实签名的合成凭据和一个已读到上界的设备。
	baselineVerifier, binding, expiresAt, masterKey := syncSnapshotBaselineVerificationFixture(t)
	baselineToken, err := baselineVerifier.signer.Sign(binding, 17, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	cursorVerifier, err := NewSyncChangeCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: binding.UserID, DeviceID: binding.DeviceID, SessionID: 7}
	position := domain.SyncChangeCursorPosition{UserID: actor.UserID, DeviceID: actor.DeviceID, Generation: binding.Generation, Sequence: 17}
	cursor, err := cursorVerifier.signer.Sign(position)
	if err != nil {
		t.Fatal(err)
	}
	state := domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: binding.Generation, AckSequence: 17}
	commitError := errors.New("fixture read transaction could not commit")
	for _, scenario := range []struct {
		name                  string
		baselineToken, cursor string
		pageSize              int
		snapshot              string
		wantCalls             int
		wantError             error
	}{
		{name: "success after read context closed", cursor: cursor, pageSize: 1, wantCalls: 1},
		{name: "invalid cursor before snapshot", cursor: cursor + "A", pageSize: 1, wantError: domain.ErrInvalidSyncChangeCursor},
		{name: "invalid page size before snapshot", cursor: cursor, pageSize: 101, wantError: domain.ErrInvalidSyncChangePage},
		{name: "expired baseline before snapshot", baselineToken: baselineToken, pageSize: 1,
			snapshot: "already expired", wantError: domain.ErrSyncSnapshotExpired},
		{name: "baseline reaches expiry in snapshot", baselineToken: baselineToken, pageSize: 1,
			snapshot: "expires in read", wantCalls: 1, wantError: domain.ErrSyncSnapshotExpired},
		{name: "missing sync record means expired", cursor: cursor, pageSize: 1,
			snapshot: "missing record", wantCalls: 1, wantError: domain.ErrSyncSnapshotExpired},
		{name: "missing events in snapshot", cursor: cursor, pageSize: 1,
			snapshot: "missing events", wantCalls: 1, wantError: domain.ErrInvalidSyncChangePage},
		{name: "commit fails after a valid read", cursor: cursor, pageSize: 1,
			snapshot: "commit fails", wantCalls: 1, wantError: commitError},
		{name: "request canceled after a valid read", cursor: cursor, pageSize: 1,
			snapshot: "cancel after read", wantCalls: 1, wantError: context.Canceled},
		{name: "snapshot callback was skipped", cursor: cursor, pageSize: 1,
			snapshot: "skip callback", wantCalls: 1, wantError: domain.ErrInvalidSyncChangePage},
		{name: "request already canceled", cursor: cursor, pageSize: 1,
			snapshot: "already canceled", wantError: context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 2. 编排只读回调，模拟事务结束、失败和请求取消。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario.snapshot == "already canceled" {
				cancel()
			}
			clockCalls := 0
			resolver := NewSyncChangePullStartResolver(baselineVerifier, cursorVerifier, func() time.Time {
				clockCalls++
				if scenario.snapshot == "already expired" || (scenario.snapshot == "expires in read" && clockCalls == 2) {
					return expiresAt
				}
				return expiresAt.Add(-time.Nanosecond)
			})
			snapshotCalls := 0
			puller := NewSyncChangePullService(resolver, func(requestContext context.Context, requested domain.Actor,
				read func(context.Context, SyncChangePageReader, domain.SyncDeviceState) error,
			) error {
				snapshotCalls++
				if requestContext != ctx || requested != actor {
					t.Fatalf("事务输入不符：身份 %+v", requested)
				}
				switch scenario.snapshot {
				case "missing record":
					return domain.ErrSyncDeviceNotFound
				case "skip callback":
					return nil
				}
				readContext, endRead := context.WithCancel(requestContext)
				defer endRead()
				reader := syncChangePullResultReader{bounds: domain.SyncChangeSequenceBounds{MinimumPosition: 17, HighWaterSequence: 17}}
				if scenario.snapshot == "missing events" {
					reader.bounds.HighWaterSequence = 18
				}
				if err := read(readContext, reader, state); err != nil {
					return err
				}
				switch scenario.snapshot {
				case "commit fails":
					return commitError
				case "cancel after read":
					cancel()
				}
				return nil
			}, cursorVerifier.signer)

			// 3. 核对事务调用次数及返回值，任何错误都不交付页面或游标。
			result, err := puller.Pull(ctx, actor, scenario.baselineToken, scenario.cursor, scenario.pageSize)
			if snapshotCalls != scenario.wantCalls {
				t.Fatalf("事务调用次数 %d，预期 %d", snapshotCalls, scenario.wantCalls)
			}
			if scenario.wantError != nil {
				if result != nil || !errors.Is(err, scenario.wantError) {
					t.Fatalf("期望错误 %v，实际结果 %+v，错误 %v", scenario.wantError, result, err)
				}
				return
			}
			if err != nil || result == nil || result.HighWaterSequence != 17 || result.HasMore ||
				result.Events == nil || len(result.Events) != 0 || result.NextCursor != cursor {
				t.Fatalf("空页应成功并保留同一游标：结果 %+v，错误 %v", result, err)
			}
		})
	}
}
