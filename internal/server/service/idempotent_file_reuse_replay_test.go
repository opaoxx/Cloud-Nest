package service

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestValidateFileReuseReplay(t *testing.T) {
	const userID, parentID, sizeBytes, key = 7, 13, 0, "reuse-001"
	requestHash := sha256.Sum256([]byte("synthetic request"))
	now := time.Date(2026, 11, 15, 8, 0, 0, 123456000, time.UTC)
	for _, scenario := range []struct {
		name      string
		change    func(*domain.IdempotencyRecord)
		wantError error
	}{
		{"one microsecond before expiry", nil, nil},
		{"expiry boundary", func(record *domain.IdempotencyRecord) { record.ExpiresAt = now }, domain.ErrIdempotencyRecordExpired},
		{"after expiry with conflicting hash", func(record *domain.IdempotencyRecord) {
			record.ExpiresAt = now.Add(-time.Microsecond)
			record.RequestHash[0] ^= 1
		}, domain.ErrIdempotencyRecordExpired},
		{"different request hash", func(record *domain.IdempotencyRecord) { record.RequestHash[0] ^= 1 }, domain.ErrIdempotencyRequestConflict},
		{"other user", func(record *domain.IdempotencyRecord) { record.UserID++ }, domain.ErrInvalidIdempotencyRecordData},
		{"other operation", func(record *domain.IdempotencyRecord) { record.Operation = domain.CreateDirectoryOperation }, domain.ErrInvalidIdempotencyRecordData},
		{"different key", func(record *domain.IdempotencyRecord) { record.IdempotencyKey = "different-key" }, domain.ErrInvalidIdempotencyRecordData},
		{"invalid status", func(record *domain.IdempotencyRecord) { record.ResponseStatus = 200 }, domain.ErrInvalidIdempotencyRecordData},
		{"invalid expiry", func(record *domain.IdempotencyRecord) { record.ExpiresAt = record.CreatedAt }, domain.ErrInvalidIdempotencyRecordData},
		{"different resource ID", func(record *domain.IdempotencyRecord) { record.ResourceID = new(uint64(30)) }, domain.ErrInvalidIdempotencyRecordData},
		{"different size", func(record *domain.IdempotencyRecord) {
			record.ResponseBody = json.RawMessage(`{"entry":{"id":"29","parent_id":"13","size_bytes":1}}`)
		}, domain.ErrInvalidIdempotencyRecordData},
		{"missing zero size", func(record *domain.IdempotencyRecord) {
			record.ResponseBody = json.RawMessage(`{"entry":{"id":"29","parent_id":"13"}}`)
		}, domain.ErrInvalidIdempotencyRecordData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 固定时间与快照关联，只改变当前用例需要核对的条件。
			record := domain.IdempotencyRecord{
				ID: 19, UserID: userID, Operation: domain.FileReuseOperation, IdempotencyKey: key, RequestHash: requestHash,
				ResourceType: "ENTRY", ResourceID: new(uint64(29)), ResponseStatus: 201,
				ResponseBody: json.RawMessage(`{"entry":{"id":"29","parent_id":"13","size_bytes":0}}`),
				CreatedAt:    now.Add(-time.Hour), ExpiresAt: now.Add(time.Microsecond),
			}
			if scenario.change != nil {
				scenario.change(&record)
			}

			// 2. 验证边界和错误优先级，完整公开字段由快照层单独核对。
			if err := validateFileReuseReplay(&record, userID, parentID, sizeBytes, key, requestHash, now); !errors.Is(err, scenario.wantError) {
				t.Fatalf("得到 %v，期望 %v", err, scenario.wantError)
			}
		})
	}
}
