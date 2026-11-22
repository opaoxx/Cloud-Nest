package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSyncSnapshotContinuationReadBoundaries(t *testing.T) {
	// 1. 用实际签发器和缓存准备一份三条目的快照，分别注入读取边界。
	masterKey := bytes.Repeat([]byte{0x61}, sha256.Size)
	signer, err := NewSyncSnapshotCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewSyncSnapshotCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	queryFailure := errors.New("fixture query failed")
	for _, testCase := range []struct {
		name                string
		change              string
		wantError           error
		wantFind, wantCache int
	}{
		{name: "tamper before querying", change: "tamper", wantError: domain.ErrInvalidSyncSnapshotCursor},
		{name: "expiration before querying", change: "expired", wantError: domain.ErrSyncSnapshotExpired},
		{name: "device has no sync row", change: "missing sync", wantError: domain.ErrSyncSnapshotExpired, wantFind: 1},
		{name: "revoked device", change: "revoked", wantError: domain.ErrDeviceRevoked, wantFind: 1},
		{name: "database fault", change: "query error", wantError: queryFailure, wantFind: 1},
		{name: "different current generation", change: "generation", wantError: domain.ErrSyncSnapshotExpired, wantFind: 1},
		{name: "missing cached snapshot", change: "missing cache", wantError: domain.ErrSyncSnapshotExpired, wantFind: 1, wantCache: 1},
		{name: "expires before cache read", change: "late expiry", wantError: domain.ErrSyncSnapshotExpired, wantFind: 1, wantCache: 1},
		{name: "signed expiration differs from cache", change: "different expiry", wantError: domain.ErrInvalidSyncSnapshotCursor, wantFind: 1, wantCache: 1},
		{name: "position exceeds cached items", change: "position", wantError: domain.ErrInvalidSyncSnapshotCursor, wantFind: 1, wantCache: 1},
		{name: "cancellation while querying", change: "cancel", wantError: context.Canceled, wantFind: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			now := time.Date(2026, 11, 23, 2, 0, 0, 123456789, time.UTC)
			clock := func() time.Time { return now }
			cache := NewSyncSnapshotCache(clock)
			binding, data := syncSnapshotCacheFixture()
			binding.PageSize = 2
			data.Entries.Items = []json.RawMessage{
				json.RawMessage(snapshotCacheFixtureItem),
				json.RawMessage(strings.Replace(snapshotCacheFixtureItem, `"entry_id":"12"`, `"entry_id":"13"`, 1)),
				json.RawMessage(strings.Replace(snapshotCacheFixtureItem, `"entry_id":"12"`, `"entry_id":"14"`, 1)),
			}
			data.Entries.SizeBytes = len("[]") + 2 + 3*len(snapshotCacheFixtureItem)
			expiresAt := putSyncSnapshotForReadTest(t, cache, binding, data)
			position := 2
			signedExpiry := expiresAt
			requestBinding := binding
			switch testCase.change {
			case "expired":
				now = expiresAt
			case "missing cache":
				requestBinding.SnapshotID[15] ^= 1
			case "different expiry":
				signedExpiry = expiresAt.Add(time.Nanosecond)
			case "position":
				position = 4
			}
			credential, err := signer.Sign(requestBinding, position, signedExpiry)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.change == "tamper" {
				credential = "A" + credential[1:]
			}

			// 2. 记录数据库及缓存是否被访问，确认拒绝条件在对应阶段停止。
			findCalls, cacheCalls := 0, 0
			reader := NewSyncSnapshotContinuationService(verifier.Verify, func(_ context.Context, userID, deviceID uint64) (domain.SyncCursorGeneration, error) {
				findCalls++
				if userID != binding.UserID || deviceID != binding.DeviceID {
					t.Fatal("查询未使用已认证的账号和设备")
				}
				generation := binding.Generation
				switch testCase.change {
				case "missing sync":
					return domain.SyncCursorGeneration{}, domain.ErrSyncDeviceNotFound
				case "revoked":
					return domain.SyncCursorGeneration{}, domain.ErrDeviceRevoked
				case "query error":
					return domain.SyncCursorGeneration{}, queryFailure
				case "generation":
					generation[15] ^= 1
				case "late expiry":
					now = expiresAt
				case "cancel":
					cancel()
				}
				return generation, nil
			}, func(requestContext context.Context, key SyncSnapshotCacheBinding) (*CachedSyncSnapshot, error) {
				cacheCalls++
				if key != requestBinding {
					t.Fatal("缓存读取未使用核验后的快照绑定")
				}
				return cache.Get(requestContext, key)
			}, clock)
			actor := domain.Actor{UserID: binding.UserID, DeviceID: binding.DeviceID, SessionID: 71}
			result, err := reader.Read(ctx, actor, requestBinding.SnapshotID, credential)
			if result != nil || !errors.Is(err, testCase.wantError) || findCalls != testCase.wantFind || cacheCalls != testCase.wantCache {
				t.Fatalf("续页边界不符，代次查询=%d，缓存读取=%d，错误=%v", findCalls, cacheCalls, err)
			}
		})
	}
}
