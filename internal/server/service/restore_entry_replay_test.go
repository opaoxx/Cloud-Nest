package service

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func restoreEntryReplayFixture(t *testing.T, entryID, revision uint64) (*domain.IdempotencyRecord, time.Time) {
	t.Helper()
	digest, err := BuildRestoreEntryRequestHash(entryID, revision)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 11, 20, 4, 30, 0, 123456000, time.UTC)
	return &domain.IdempotencyRecord{
		ID: 42, UserID: 7, Operation: domain.RestoreEntryOperation, IdempotencyKey: "restore-request-007",
		RequestHash: digest, ResourceType: "ENTRY", ResourceID: &entryID, ResponseStatus: 200,
		ResponseBody: []byte(`{"entry":{"id":"23","parent_id":"11","name":"学习笔记 (1).md","entry_type":"FILE","status":"ACTIVE","revision":"5"}}`),
		CreatedAt:    createdAt, ExpiresAt: createdAt.Add(8 * 24 * time.Hour),
	}, createdAt.Add(time.Hour)
}

func TestValidateRestoreEntryReplayValid(t *testing.T) {
	for _, scenario := range []struct {
		name              string
		entryID, revision uint64
		body              string
	}{
		{"normal fallback snapshot", 23, 4, ""},
		{"reordered fields", 23, 4, `{
			"entry": {"revision":"5","id":"23","name":"学习笔记.md","entry_type":"FILE","status":"ACTIVE","parent_id":"12"}
		}`},
		{"maximum result version and identifiers", math.MaxUint64, math.MaxUint64 - 1,
			`{"entry":{"id":"18446744073709551615","parent_id":"18446744073709551614","name":"资料","entry_type":"DIR","status":"ACTIVE","revision":"18446744073709551615"}}`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			record, _ := restoreEntryReplayFixture(t, scenario.entryID, scenario.revision)
			if scenario.body != "" {
				record.ResponseBody = []byte(scenario.body)
			}
			before := *record
			before.ResourceID = new(*record.ResourceID)
			before.ResponseBody = bytes.Clone(record.ResponseBody)
			// 截止时间前一纳秒仍可复用，记录保留原值。
			now := record.ExpiresAt.Add(-time.Nanosecond).In(time.FixedZone("UTC+08", 8*3600))
			if err := ValidateRestoreEntryReplay(record, 7, scenario.entryID, scenario.revision, record.IdempotencyKey, record.RequestHash, now); err != nil ||
				!reflect.DeepEqual(record, &before) {
				t.Fatal("合法记录未通过或记录被修改", err)
			}
		})
	}
}

func TestValidateRestoreEntryReplayErrorOrder(t *testing.T) {
	for _, mode := range []string{"different target", "different version", "at expiration with mismatch", "wrong scope before expiration", "maximum version with mismatch", "maximum version with matching hash"} {
		t.Run(mode, func(t *testing.T) {
			record, now := restoreEntryReplayFixture(t, 23, 4)
			entryID, revision := uint64(23), uint64(4)
			want := domain.ErrIdempotencyRequestConflict
			switch mode {
			case "different target":
				entryID = 24
			case "different version":
				revision = 5
			case "at expiration with mismatch":
				revision, now, want = 5, record.ExpiresAt, domain.ErrIdempotencyRecordExpired
			case "wrong scope before expiration":
				record.Operation, now, want = domain.TrashEntryOperation, record.ExpiresAt, domain.ErrInvalidIdempotencyRecordData
			case "maximum version with mismatch":
				revision = math.MaxUint64
			case "maximum version with matching hash":
				revision, want = math.MaxUint64, domain.ErrInvalidIdempotencyRecordData
			}
			digest, err := BuildRestoreEntryRequestHash(entryID, revision)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "maximum version with matching hash" {
				record.RequestHash = digest
			}
			if err := ValidateRestoreEntryReplay(record, 7, entryID, revision, record.IdempotencyKey, digest, now); !errors.Is(err, want) {
				t.Fatalf("当前返回 %v，期望 %v", err, want)
			}
		})
	}
}

func TestValidateRestoreEntryReplayRejectsInvalidRecord(t *testing.T) {
	for _, scenario := range []struct {
		name string
		edit func(*domain.IdempotencyRecord)
	}{
		{"zero record ID", func(record *domain.IdempotencyRecord) { record.ID = 0 }},
		{"foreign user", func(record *domain.IdempotencyRecord) { record.UserID = 8 }},
		{"different key case", func(record *domain.IdempotencyRecord) { record.IdempotencyKey = "Restore-request-007" }},
		{"missing creation time", func(record *domain.IdempotencyRecord) { record.CreatedAt = time.Time{} }},
		{"invalid time order", func(record *domain.IdempotencyRecord) { record.CreatedAt = record.ExpiresAt }},
		{"different resource", func(record *domain.IdempotencyRecord) { record.ResourceType = "UPLOAD_TASK" }},
		{"missing resource ID", func(record *domain.IdempotencyRecord) { record.ResourceID = nil }},
		{"different resource ID", func(record *domain.IdempotencyRecord) { *record.ResourceID = 24 }},
		{"different HTTP status", func(record *domain.IdempotencyRecord) { record.ResponseStatus = 201 }},
		{"missing JSON", func(record *domain.IdempotencyRecord) { record.ResponseBody = nil }},
		{"snapshot target mismatch", func(record *domain.IdempotencyRecord) {
			record.ResponseBody = bytes.Replace(record.ResponseBody, []byte(`"id":"23"`), []byte(`"id":"24"`), 1)
		}},
		{"snapshot version mismatch", func(record *domain.IdempotencyRecord) {
			record.ResponseBody = bytes.Replace(record.ResponseBody, []byte(`"revision":"5"`), []byte(`"revision":"6"`), 1)
		}},
		{"invalid UTF-8", func(record *domain.IdempotencyRecord) { record.ResponseBody = append(record.ResponseBody, 0xff) }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			record, now := restoreEntryReplayFixture(t, 23, 4)
			scenario.edit(record)
			if err := ValidateRestoreEntryReplay(record, 7, 23, 4, "restore-request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatal("异常记录未被拒绝", err)
			}
		})
	}
}

func TestValidateRestoreEntryReplayInputs(t *testing.T) {
	record, now := restoreEntryReplayFixture(t, 23, 4)
	for _, scenario := range []struct {
		userID, entryID, revision uint64
		key                       string
		at                        time.Time
	}{
		{0, 23, 4, record.IdempotencyKey, now},
		{7, 0, 4, record.IdempotencyKey, now},
		{7, 23, 0, record.IdempotencyKey, now},
		{7, 23, 4, "invalid,key", now},
		{7, 23, 4, record.IdempotencyKey, time.Time{}},
	} {
		if err := ValidateRestoreEntryReplay(record, scenario.userID, scenario.entryID, scenario.revision, scenario.key, record.RequestHash, scenario.at); err == nil ||
			errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || errors.Is(err, domain.ErrIdempotencyRequestConflict) {
			t.Fatal("无效调用参数未报错或被归为记录错误", err)
		}
	}
}
