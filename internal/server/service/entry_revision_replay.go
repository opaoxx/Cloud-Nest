package service

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// validateEntryRevisionReplay 核对回收和恢复的记录与当前目录项请求的关联。
func validateEntryRevisionReplay(record *domain.IdempotencyRecord, userID, entryID, expectedRevision uint64, operation, key string, requestHash [sha256.Size]byte, now time.Time) error {
	// 1. 核对记录范围、到期时间和请求摘要。
	if err := validateEntryReplayRecord(record, userID, operation, key, requestHash, now); err != nil {
		return err
	}

	// 2. 确认保存的是成功结果，且目标一致、结果版本等于预期版本加一。
	if record.ResourceType != "ENTRY" || record.ResourceID == nil || *record.ResourceID != entryID || record.ResponseStatus != 200 ||
		expectedRevision == math.MaxUint64 || !utf8.Valid(record.ResponseBody) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	// 完整公开字段由对应的快照解码器校验。
	var body struct {
		Entry *struct {
			ID       string `json:"id"`
			Revision string `json:"revision"`
		} `json:"entry"`
	}
	if json.Unmarshal(record.ResponseBody, &body) != nil || body.Entry == nil ||
		body.Entry.ID != strconv.FormatUint(entryID, 10) || body.Entry.Revision != strconv.FormatUint(expectedRevision+1, 10) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	return nil
}

// validateEntryReplayRecord 核对目录项操作的记录范围、期限和请求摘要。
func validateEntryReplayRecord(record *domain.IdempotencyRecord, userID uint64, operation, key string, requestHash [sha256.Size]byte, now time.Time) error {
	// 1. 核对用户、操作和幂等键。
	if record == nil || record.ID == 0 || record.UserID != userID || record.Operation != operation || record.IdempotencyKey != key {
		return domain.ErrInvalidIdempotencyRecordData
	}

	// 2. 先检查是否到期，再比对请求摘要。
	createdAt, expiresAt := record.CreatedAt.UTC(), record.ExpiresAt.UTC()
	if createdAt.Year() < 1000 || createdAt.Year() > 9999 || expiresAt.Year() < 1000 || expiresAt.Year() > 9999 || !expiresAt.After(createdAt) {
		return domain.ErrInvalidIdempotencyRecordData
	}
	if !now.Before(expiresAt) {
		return domain.ErrIdempotencyRecordExpired
	}
	if record.RequestHash != requestHash {
		return domain.ErrIdempotencyRequestConflict
	}

	return nil
}
