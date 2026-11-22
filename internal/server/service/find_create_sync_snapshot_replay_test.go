package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestFindCreateSyncSnapshotReplayInTransaction(t *testing.T) {
	queryFailure := errors.New("fixture query failed")
	for _, testCase := range []struct {
		name        string
		want        error
		wantLocator bool
		wantLookup  int
		wantDecode  int
	}{
		{name: "no record", wantLookup: 1},
		{name: "same request", wantLocator: true, wantLookup: 1, wantDecode: 1},
		{name: "different page size", want: domain.ErrIdempotencyRequestConflict, wantLookup: 1},
		{name: "expired record", want: domain.ErrIdempotencyRecordExpired, wantLookup: 1},
		{name: "query failed", want: queryFailure, wantLookup: 1},
		{name: "missing with record", want: domain.ErrInvalidIdempotencyRecordData, wantLookup: 1},
		{name: "nil record without error", want: domain.ErrInvalidIdempotencyRecordData, wantLookup: 1},
		{name: "invalid receipt body", want: domain.ErrInvalidIdempotencyRecordData, wantLookup: 1, wantDecode: 1},
		{name: "canceled before lookup", want: context.Canceled},
		{name: "canceled after lookup", want: context.Canceled, wantLookup: 1},
		{name: "canceled after decode", want: context.Canceled, wantLookup: 1, wantDecode: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 准备同一账号的真实摘要和回执正文，再改变一个条件。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			record, actor, expectedLocator := syncSnapshotReplayFixture(t)
			requestHash, now := record.RequestHash, record.CreatedAt
			switch testCase.name {
			case "different page size":
				var err error
				requestHash, err = service.BuildCreateSyncSnapshotRequestHash(actor.DeviceID, syncSnapshotReplayPageSize+1)
				if err != nil {
					t.Fatal(err)
				}
			case "expired record":
				now = record.ExpiresAt
			case "invalid receipt body":
				record.ResponseBody = json.RawMessage(`{}`)
			case "canceled before lookup":
				cancel()
			}

			// 2. 验证事务查询的范围和调用顺序，只有有效命中才交付定位数据。
			lookupCalls, decodeCalls := 0, 0
			locator, err := service.FindCreateSyncSnapshotReplayInTransaction(ctx, actor, syncSnapshotReplayKey, requestHash, now,
				func(queryContext context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error) {
					lookupCalls++
					if queryContext != ctx || userID != actor.UserID || operation != domain.CreateSyncSnapshotOperation || key != syncSnapshotReplayKey {
						t.Fatal("查询回执使用了错误的上下文或范围")
					}
					switch testCase.name {
					case "no record":
						return nil, domain.ErrIdempotencyRecordNotFound
					case "query failed":
						return nil, queryFailure
					case "missing with record":
						return &record, domain.ErrIdempotencyRecordNotFound
					case "nil record without error":
						return nil, nil
					case "canceled after lookup":
						cancel()
					}
					return &record, nil
				}, func(body json.RawMessage) (*domain.SyncSnapshotReceiptLocator, error) {
					decodeCalls++
					if testCase.name == "canceled after decode" {
						cancel()
					}
					return dto.DecodeCreateSyncSnapshotReceipt(body)
				})
			if !errors.Is(err, testCase.want) || lookupCalls != testCase.wantLookup || decodeCalls != testCase.wantDecode ||
				(locator != nil) != testCase.wantLocator || (locator != nil && *locator != expectedLocator) {
				t.Fatalf("定位=%+v，错误=%v，查询=%d，解码=%d", locator, err, lookupCalls, decodeCalls)
			}
		})
	}
}
