package service_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

const syncSnapshotReplayKey = "Snapshot-Create-A"
const syncSnapshotReplayPageSize = 50

func syncSnapshotReplayFixture(t *testing.T) (domain.IdempotencyRecord, domain.Actor, domain.SyncSnapshotReceiptLocator) {
	t.Helper()
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
	locator := domain.SyncSnapshotReceiptLocator{
		SnapshotID: uuid.MustParse("052dc4f7-f4c1-46b3-a509-bf96992f6580"),
		DeviceID:   actor.DeviceID,
		Generation: domain.SyncCursorGeneration{1, 0xff},
	}
	body, err := dto.EncodeCreateSyncSnapshotReceipt(locator)
	if err != nil {
		t.Fatal(err)
	}
	requestHash, err := service.BuildCreateSyncSnapshotRequestHash(actor.DeviceID, syncSnapshotReplayPageSize)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 11, 23, 2, 0, 0, 0, time.UTC)
	return domain.IdempotencyRecord{
		ID: 23, UserID: actor.UserID, Operation: domain.CreateSyncSnapshotOperation, IdempotencyKey: syncSnapshotReplayKey,
		RequestHash: requestHash, ResourceType: domain.SyncSnapshotResourceType, ResponseStatus: http.StatusCreated,
		ResponseBody: body, CreatedAt: createdAt, ExpiresAt: createdAt.Add(8 * 24 * time.Hour),
	}, actor, locator
}

func TestValidateCreateSyncSnapshotReplaySuccess(t *testing.T) {
	// 1. 同账号同设备重新登录后，仍能复核原回执。
	record, actor, wantLocator := syncSnapshotReplayFixture(t)
	actor.SessionID++
	for _, now := range []time.Time{record.CreatedAt, record.ExpiresAt.Add(-time.Nanosecond)} {
		locator, err := service.ValidateCreateSyncSnapshotReplay(
			&record, actor, syncSnapshotReplayKey, record.RequestHash, now, dto.DecodeCreateSyncSnapshotReceipt,
		)
		if err != nil || locator == nil || *locator != wantLocator {
			t.Fatalf("复核结果为 %+v，错误 %v，期望 %+v", locator, err, wantLocator)
		}
	}
}

func TestValidateCreateSyncSnapshotReplayScopeExpiryAndConflict(t *testing.T) {
	for _, testCase := range []struct {
		name                  string
		changeRecord          func(*domain.IdempotencyRecord)
		changeDevice, expired bool
		changePageSize        bool
		want                  error
	}{
		{name: "wrong owner before expiry", changeRecord: func(record *domain.IdempotencyRecord) { record.UserID++ },
			expired: true, changePageSize: true, want: domain.ErrInvalidIdempotencyRecordData},
		{name: "wrong operation", changeRecord: func(record *domain.IdempotencyRecord) { record.Operation = domain.CreateUploadOperation },
			want: domain.ErrInvalidIdempotencyRecordData},
		{name: "wrong key case", changeRecord: func(record *domain.IdempotencyRecord) { record.IdempotencyKey = "snapshot-create-a" },
			want: domain.ErrInvalidIdempotencyRecordData},
		{name: "invalid TTL", changeRecord: func(record *domain.IdempotencyRecord) { record.ExpiresAt = record.CreatedAt },
			want: domain.ErrInvalidIdempotencyRecordData},
		{name: "expiry takes precedence over conflict", expired: true, changePageSize: true, want: domain.ErrIdempotencyRecordExpired},
		{name: "different device", changeDevice: true, want: domain.ErrIdempotencyRequestConflict},
		{name: "different page size", changePageSize: true, want: domain.ErrIdempotencyRequestConflict},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 构造本次请求摘要，按场景制造归属、期限或参数差异。
			record, actor, _ := syncSnapshotReplayFixture(t)
			now := record.ExpiresAt.Add(-time.Nanosecond)
			if testCase.expired {
				now = record.ExpiresAt
			}
			if testCase.changeRecord != nil {
				testCase.changeRecord(&record)
			}
			if testCase.changeDevice {
				actor.DeviceID++
			}
			pageSize := syncSnapshotReplayPageSize
			if testCase.changePageSize {
				pageSize++
			}
			requestHash, err := service.BuildCreateSyncSnapshotRequestHash(actor.DeviceID, pageSize)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 核对错误优先级，拒绝时不进入正文解码。
			locator, err := service.ValidateCreateSyncSnapshotReplay(&record, actor, syncSnapshotReplayKey, requestHash, now,
				func(json.RawMessage) (*domain.SyncSnapshotReceiptLocator, error) {
					t.Fatal("拒绝回执后仍解码正文")
					return nil, nil
				})
			if locator != nil || !errors.Is(err, testCase.want) {
				t.Fatalf("定位数据为 %+v，错误 %v，期望 %v", locator, err, testCase.want)
			}
		})
	}
}

func TestValidateCreateSyncSnapshotReplayRejectsInvalidReceipt(t *testing.T) {
	record, actor, otherLocator := syncSnapshotReplayFixture(t)
	otherLocator.DeviceID++
	otherDeviceBody, err := dto.EncodeCreateSyncSnapshotReceipt(otherLocator)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name    string
		change  func(*domain.IdempotencyRecord)
		decodes bool
	}{
		{"wrong resource type", func(record *domain.IdempotencyRecord) { record.ResourceType = "ENTRY" }, false},
		{"unexpected numeric resource ID", func(record *domain.IdempotencyRecord) { record.ResourceID = new(uint64(19)) }, false},
		{"wrong status", func(record *domain.IdempotencyRecord) { record.ResponseStatus = http.StatusOK }, false},
		{"absent response body", func(record *domain.IdempotencyRecord) { record.ResponseBody = nil }, false},
		{"invalid JSON body", func(record *domain.IdempotencyRecord) { record.ResponseBody = json.RawMessage("{") }, true},
		{"device mismatch in saved body", func(record *domain.IdempotencyRecord) { record.ResponseBody = otherDeviceBody }, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			updated := record
			testCase.change(&updated)
			called := false
			locator, err := service.ValidateCreateSyncSnapshotReplay(
				&updated, actor, syncSnapshotReplayKey, record.RequestHash, record.CreatedAt,
				func(body json.RawMessage) (*domain.SyncSnapshotReceiptLocator, error) {
					called = true
					return dto.DecodeCreateSyncSnapshotReceipt(body)
				},
			)
			if locator != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || called != testCase.decodes {
				t.Fatalf("定位数据为 %+v，错误 %v，调用解码=%t", locator, err, called)
			}
		})
	}
}
