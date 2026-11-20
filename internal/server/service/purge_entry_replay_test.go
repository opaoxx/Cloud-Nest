package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func purgeEntryReplayFixture(t *testing.T, entryID uint64) *domain.IdempotencyRecord {
	t.Helper()
	hash, err := BuildPurgeEntryRequestHash(entryID, 4, purgeHashExampleDigest)
	if err != nil {
		t.Fatal(err)
	}
	parentID := uint64(11)
	data, err := dto.NewPurgeEntryAcceptedData(&domain.FileEntry{
		ID: entryID, UserID: 7, ParentID: &parentID, Revision: 5, Status: "PURGING",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 11, 20, 4, 30, 0, 123456000, time.UTC)
	return &domain.IdempotencyRecord{
		ID: 42, UserID: 7, Operation: domain.PurgeEntryOperation, IdempotencyKey: "purge-request-007",
		RequestHash: hash, ResourceType: "ENTRY", ResourceID: &entryID, ResponseStatus: 202,
		ResponseBody: body, CreatedAt: createdAt, ExpiresAt: createdAt.Add(8 * 24 * time.Hour),
	}
}

func decodePurgeReplayTarget(body []byte) (string, error) {
	data, err := dto.DecodePurgeEntryAcceptedData(body)
	if err != nil {
		return "", err
	}
	return data.EntryID, nil
}

func TestValidatePurgeEntryReplayValid(t *testing.T) {
	// 1. 截止前一纳秒仍可复用，并保留完整目标 ID 和原记录。
	for _, entryID := range []uint64{23, math.MaxUint64} {
		record := purgeEntryReplayFixture(t, entryID)
		before := *record
		copiedID := *record.ResourceID
		before.ResourceID = &copiedID
		before.ResponseBody = bytes.Clone(record.ResponseBody)
		now := record.ExpiresAt.Add(-time.Nanosecond).In(time.FixedZone("UTC+08", 8*3600))
		if err := ValidatePurgeEntryReplay(record, 7, entryID, record.IdempotencyKey, record.RequestHash, now, decodePurgeReplayTarget); err != nil ||
			!reflect.DeepEqual(*record, before) {
			t.Fatal("合法受理记录未通过或记录被修改", err)
		}
	}
}

func TestValidatePurgeEntryReplayErrorOrder(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		foreignUser bool
		expired     bool
		want        error
	}{
		{"same key different credential", false, false, domain.ErrIdempotencyRequestConflict},
		{"expired before hash comparison", false, true, domain.ErrIdempotencyRecordExpired},
		{"invalid scope before expiry", true, true, domain.ErrInvalidIdempotencyRecordData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			record := purgeEntryReplayFixture(t, 23)
			now := record.CreatedAt.Add(time.Hour)
			if scenario.expired {
				now = record.ExpiresAt
			}
			if scenario.foreignUser {
				record.UserID++
			}
			hash, err := BuildPurgeEntryRequestHash(23, 4, "f"+purgeHashExampleDigest[1:])
			if err != nil {
				t.Fatal(err)
			}
			decoder := func([]byte) (string, error) {
				t.Fatal("范围、期限或摘要拒绝后应停止解码")
				return "", nil
			}
			if err := ValidatePurgeEntryReplay(record, 7, 23, record.IdempotencyKey, hash, now, decoder); !errors.Is(err, scenario.want) {
				t.Fatal("幂等错误优先级不符", err, scenario.want)
			}
		})
	}
}

func TestValidatePurgeEntryReplayRejectsInvalidRecord(t *testing.T) {
	for _, scenario := range []struct {
		name string
		edit func(*domain.IdempotencyRecord)
	}{
		{"wrong operation", func(record *domain.IdempotencyRecord) { record.Operation = domain.RestoreEntryOperation }},
		{"wrong key case", func(record *domain.IdempotencyRecord) { record.IdempotencyKey = "Purge-request-007" }},
		{"zero record ID", func(record *domain.IdempotencyRecord) { record.ID = 0 }},
		{"invalid time order", func(record *domain.IdempotencyRecord) { record.CreatedAt = record.ExpiresAt }},
		{"wrong resource type", func(record *domain.IdempotencyRecord) { record.ResourceType = "UPLOAD_TASK" }},
		{"missing resource ID", func(record *domain.IdempotencyRecord) { record.ResourceID = nil }},
		{"wrong resource ID", func(record *domain.IdempotencyRecord) { *record.ResourceID = 24 }},
		{"wrong HTTP status", func(record *domain.IdempotencyRecord) { record.ResponseStatus = 200 }},
		{"missing snapshot", func(record *domain.IdempotencyRecord) { record.ResponseBody = nil }},
		{"malformed snapshot", func(record *domain.IdempotencyRecord) { record.ResponseBody = []byte("null") }},
		{"invalid progress path", func(record *domain.IdempotencyRecord) {
			record.ResponseBody = bytes.Replace(record.ResponseBody, []byte("/23/purge"), []byte("/24/purge"), 1)
		}},
		{"valid snapshot for another target", func(record *domain.IdempotencyRecord) {
			record.ResponseBody = bytes.ReplaceAll(record.ResponseBody, []byte("23"), []byte("24"))
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			record := purgeEntryReplayFixture(t, 23)
			scenario.edit(record)
			if err := ValidatePurgeEntryReplay(record, 7, 23, "purge-request-007", record.RequestHash, record.CreatedAt, decodePurgeReplayTarget); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatal("无效幂等受理记录未被拒绝", err)
			}
		})
	}
}

func TestValidatePurgeEntryReplayRejectsInvalidInputs(t *testing.T) {
	record := purgeEntryReplayFixture(t, 23)
	for _, scenario := range []struct {
		userID, entryID uint64
		key             string
		now             time.Time
		decoder         func([]byte) (string, error)
	}{
		{0, 23, record.IdempotencyKey, record.CreatedAt, decodePurgeReplayTarget},
		{7, 0, record.IdempotencyKey, record.CreatedAt, decodePurgeReplayTarget},
		{7, 23, "bad,key", record.CreatedAt, decodePurgeReplayTarget},
		{7, 23, record.IdempotencyKey, time.Time{}, decodePurgeReplayTarget},
		{7, 23, record.IdempotencyKey, record.CreatedAt, nil},
	} {
		if err := ValidatePurgeEntryReplay(record, scenario.userID, scenario.entryID, scenario.key, [sha256.Size]byte{}, scenario.now, scenario.decoder); err == nil ||
			errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || errors.Is(err, domain.ErrIdempotencyRequestConflict) {
			t.Fatal("无效调用应先于已存记录校验被拒绝", err)
		}
	}
}
