package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func trashEntryReplayFixture(t *testing.T) (*domain.IdempotencyRecord, time.Time) {
	t.Helper()
	digest, err := BuildTrashEntryRequestHash(23, 3)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 11, 20, 4, 30, 0, 123456000, time.UTC)
	return &domain.IdempotencyRecord{
		ID: 42, UserID: 7, Operation: domain.TrashEntryOperation, IdempotencyKey: "request-007",
		RequestHash: digest, ResourceType: "ENTRY", ResourceID: new(uint64(23)), ResponseStatus: 200,
		ResponseBody: json.RawMessage(`{"entry":{"id":"23","revision":"4"}}`),
		CreatedAt:    createdAt, ExpiresAt: createdAt.Add(8 * 24 * time.Hour),
	}, createdAt.Add(time.Hour)
}

func TestValidateTrashEntryReplayValid(t *testing.T) {
	for _, mode := range []string{"ordinary", "before deadline", "time zone", "maximum result version and IDs", "JSON reordered", "maximum key"} {
		t.Run(mode, func(t *testing.T) {
			record, now := trashEntryReplayFixture(t)
			expectedRevision := uint64(3)
			switch mode {
			case "before deadline":
				now = record.ExpiresAt.Add(-time.Nanosecond)
			case "time zone":
				zone := time.FixedZone("UTC+08", 8*3600)
				record.CreatedAt, record.ExpiresAt, now = record.CreatedAt.In(zone), record.ExpiresAt.In(zone), now.In(zone)
			case "maximum result version and IDs":
				record.ID, record.UserID, *record.ResourceID, expectedRevision = math.MaxUint64, math.MaxUint64, math.MaxUint64, math.MaxUint64-1
				record.ResponseBody = json.RawMessage(`{"entry":{"id":"18446744073709551615","revision":"18446744073709551615"}}`)
				digest, err := BuildTrashEntryRequestHash(*record.ResourceID, expectedRevision)
				if err != nil {
					t.Fatal(err)
				}
				record.RequestHash = digest
			case "JSON reordered":
				record.ResponseBody = json.RawMessage("{\n  \"entry\": { \"revision\": \"4\", \"id\": \"23\" } \n}")
			case "maximum key":
				record.IdempotencyKey = strings.Repeat("K", 128)
			}
			before := *record
			before.ResourceID = new(*record.ResourceID)
			before.ResponseBody = bytes.Clone(record.ResponseBody)

			// 1. 校验保留有效记录，确认原记录各字段原样保留。
			err := ValidateTrashEntryReplay(record, record.UserID, *record.ResourceID, expectedRevision, record.IdempotencyKey, record.RequestHash, now)
			if err != nil || !reflect.DeepEqual(record, &before) {
				t.Fatal("合法幂等记录未通过或记录被修改", err)
			}
		})
	}
}

func TestValidateTrashEntryReplayInvalidRecord(t *testing.T) {
	for _, scenario := range []struct {
		name string
		edit func(*domain.IdempotencyRecord)
	}{
		{"zero record ID", func(record *domain.IdempotencyRecord) { record.ID = 0 }},
		{"foreign account", func(record *domain.IdempotencyRecord) { record.UserID = 8 }},
		{"other operation", func(record *domain.IdempotencyRecord) { record.Operation = domain.MoveEntryOperation }},
		{"case-sensitive key", func(record *domain.IdempotencyRecord) { record.IdempotencyKey = "Request-007" }},
		{"missing creation time", func(record *domain.IdempotencyRecord) { record.CreatedAt = time.Time{} }},
		{"creation after expiration", func(record *domain.IdempotencyRecord) { record.CreatedAt = record.ExpiresAt.Add(time.Microsecond) }},
		{"expiration equals creation", func(record *domain.IdempotencyRecord) { record.ExpiresAt = record.CreatedAt }},
		{"expiration outside UTC range", func(record *domain.IdempotencyRecord) {
			record.ExpiresAt = time.Date(9999, 12, 31, 23, 59, 0, 0, time.FixedZone("UTC-1", -3600))
		}},
		{"other resource type", func(record *domain.IdempotencyRecord) { record.ResourceType = "UPLOAD_TASK" }},
		{"missing resource ID", func(record *domain.IdempotencyRecord) { record.ResourceID = nil }},
		{"zero resource ID", func(record *domain.IdempotencyRecord) { *record.ResourceID = 0 }},
		{"other resource ID", func(record *domain.IdempotencyRecord) { *record.ResourceID = 24 }},
		{"non-200 status", func(record *domain.IdempotencyRecord) { record.ResponseStatus = 201 }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			record, now := trashEntryReplayFixture(t)
			scenario.edit(record)
			if err := ValidateTrashEntryReplay(record, 7, 23, 3, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatal("异常记录元数据未拒绝", err)
			}
		})
	}
	record, now := trashEntryReplayFixture(t)
	if err := ValidateTrashEntryReplay(nil, 7, 23, 3, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
		t.Fatal("空记录未拒绝", err)
	}
	for _, body := range []string{
		"", "null", "[]", `{"entry":null}`, `{"entry":[]}`, `{"entry":{"id":"23"}}`,
		`{"entry":{"id":"24","revision":"4"}}`, `{"entry":{"id":"023","revision":"4"}}`,
		`{"entry":{"id":23,"revision":"4"}}`, `{"entry":{"id":"23","revision":"3"}}`,
		`{"entry":{"id":"23","revision":"04"}}`, `{"entry":{"id":"23","revision":4}}`,
		`{"entry":{"id":"23","revision":"4"}} {}`, "{\"entry\":{\"id\":\"23\",\"revision\":\"4\",\"name\":\"\xff\"}}",
	} {
		t.Run(body, func(t *testing.T) {
			record.ResponseBody = json.RawMessage(body)
			if err := ValidateTrashEntryReplay(record, 7, 23, 3, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatal("异常正文或结果关联未拒绝", err)
			}
		})
	}
}

func TestValidateTrashEntryReplayErrorOrder(t *testing.T) {
	for _, mode := range []string{"different target", "different revision", "maximum requested revision", "expired at deadline", "invalid scope before expiry", "overflowing saved result"} {
		t.Run(mode, func(t *testing.T) {
			record, now := trashEntryReplayFixture(t)
			entryID, revision := uint64(23), uint64(3)
			want := domain.ErrIdempotencyRequestConflict
			switch mode {
			case "different target":
				entryID = 24
			case "different revision":
				revision = 4
			case "maximum requested revision":
				revision = math.MaxUint64
			case "expired at deadline":
				revision, now, want = 4, record.ExpiresAt, domain.ErrIdempotencyRecordExpired
			case "invalid scope before expiry":
				record.UserID, revision, now, want = 8, 4, record.ExpiresAt, domain.ErrInvalidIdempotencyRecordData
			case "overflowing saved result":
				revision, want = math.MaxUint64, domain.ErrInvalidIdempotencyRecordData
				record.ResponseBody = json.RawMessage(`{"entry":{"id":"23","revision":"0"}}`)
			}
			digest, err := BuildTrashEntryRequestHash(entryID, revision)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "overflowing saved result" {
				record.RequestHash = digest
			}
			if err := ValidateTrashEntryReplay(record, 7, entryID, revision, "request-007", digest, now); !errors.Is(err, want) {
				t.Fatal("回收幂等错误顺序或版本边界不符", err)
			}
		})
	}
}

func TestValidateTrashEntryReplayInputs(t *testing.T) {
	record, now := trashEntryReplayFixture(t)
	for _, scenario := range []struct {
		userID, entryID, revision uint64
		key                       string
		at                        time.Time
	}{
		{0, 23, 3, record.IdempotencyKey, now},
		{7, 0, 3, record.IdempotencyKey, now},
		{7, 23, 0, record.IdempotencyKey, now},
		{7, 23, 3, "invalid,key", now},
		{7, 23, 3, strings.Repeat("k", 129), now},
		{7, 23, 3, record.IdempotencyKey, time.Time{}},
	} {
		err := ValidateTrashEntryReplay(record, scenario.userID, scenario.entryID, scenario.revision, scenario.key, record.RequestHash, scenario.at)
		if err == nil || errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || errors.Is(err, domain.ErrIdempotencyRequestConflict) {
			t.Fatal("调用参数错误未与持久记录错误区分", err)
		}
	}
}
