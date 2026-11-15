package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// FindUserIdempotencyRecord 按用户、服务端操作名和已解析幂等键读取记录。
// 返回的到期时间与结果字段由后续服务解释，缺失返回 ErrIdempotencyRecordNotFound。
// 组合业务写入时，调用方在已认证用户事务中复用当前 Store。
func (store *Store) FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error) {
	// 1. 检查查询范围和上下文，设置查询期限。
	if ctx == nil || userID == 0 || len(operation) == 0 || len(operation) > 64 || len(key) == 0 || len(key) > 128 {
		return nil, fmt.Errorf("查询幂等记录需要上下文、非零用户 ID、1～64 字节操作名和 1～128 字节幂等键")
	}
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询幂等记录失败: %w", err)
	}

	// 2. 按用户、操作名及幂等键读取指定字段。
	var record models.IdempotencyRecord
	queryError := store.database.WithContext(queryContext).
		Select("id", "user_id", "operation", "idempotency_key", "request_hash", "resource_type", "resource_id", "response_status", "response_body", "expires_at", "created_at").
		Where("user_id = ? AND operation = ? AND idempotency_key = ?", userID, operation, key).
		Take(&record).Error

	// 3. 处理取消、未找到及数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询幂等记录已取消: %w", err)
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrIdempotencyRecordNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询幂等记录失败: %w", queryError)
	}

	// 4. 核对记录并复制摘要、资源 ID 和响应正文。
	if record.ID == 0 || record.UserID != userID || record.Operation != operation || record.IdempotencyKey != key ||
		len(record.RequestHash) != sha256.Size || record.ResourceType == "" || (record.ResourceID != nil && *record.ResourceID == 0) ||
		record.ResponseStatus < 100 || record.ResponseStatus > 599 || record.ExpiresAt.IsZero() || record.CreatedAt.IsZero() {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	if record.ResponseBody != nil && (!utf8.Valid(record.ResponseBody) || !json.Valid(record.ResponseBody)) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	var requestHash [sha256.Size]byte
	copy(requestHash[:], record.RequestHash)
	var resourceID *uint64
	if record.ResourceID != nil {
		resourceCopy := *record.ResourceID
		resourceID = &resourceCopy
	}
	return &domain.IdempotencyRecord{
		ID:             record.ID,
		UserID:         record.UserID,
		Operation:      record.Operation,
		IdempotencyKey: record.IdempotencyKey,
		RequestHash:    requestHash,
		ResourceType:   record.ResourceType,
		ResourceID:     resourceID,
		ResponseStatus: record.ResponseStatus,
		ResponseBody:   json.RawMessage(bytes.Clone(record.ResponseBody)),
		ExpiresAt:      record.ExpiresAt,
		CreatedAt:      record.CreatedAt,
	}, nil
}
