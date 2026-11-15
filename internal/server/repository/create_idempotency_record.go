package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// CreateUserIdempotencyRecord 在 WithUserTransaction 回调中保存稳定业务结果。
// 用户归属来自事务身份，输入由业务服务组装，回调须将错误交回事务入口。
// 返回值在外层事务提交成功后确认生效，重复范围返回 ErrIdempotencyRecordAlreadyExists。
func (store *Store) CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error) {
	// 1. 检查已认证用户事务和上下文。
	if ctx == nil || store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("保存幂等结果需要上下文和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("保存幂等结果必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	writeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := writeContext.Err(); err != nil {
		return nil, err
	}

	// 2. 校验并复制待保存字段，规范化到期时间。
	invalidASCII := func(character rune) bool { return character < 0x21 || character > 0x7e }
	if len(input.Operation) == 0 || len(input.Operation) > 64 || strings.ContainsFunc(input.Operation, invalidASCII) ||
		!domain.ValidIdempotencyKey(input.IdempotencyKey) || len(input.ResourceType) == 0 || len(input.ResourceType) > 32 ||
		strings.ContainsFunc(input.ResourceType, invalidASCII) || input.ResponseStatus < 100 || input.ResponseStatus > 599 {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	record := models.NewIdempotencyRecord{
		UserID:         actor.UserID,
		Operation:      input.Operation,
		IdempotencyKey: input.IdempotencyKey,
		RequestHash:    bytes.Clone(input.RequestHash[:]),
		ResourceType:   input.ResourceType,
		ResponseStatus: input.ResponseStatus,
		ExpiresAt:      input.ExpiresAt.UTC().Truncate(time.Microsecond),
	}
	if input.ResourceID != nil {
		resourceCopy := *input.ResourceID
		if resourceCopy == 0 {
			return nil, domain.ErrInvalidIdempotencyRecordData
		}
		record.ResourceID = &resourceCopy
	}
	if input.ResponseBody != nil {
		bodyCopy := string(input.ResponseBody)
		if !utf8.ValidString(bodyCopy) || !json.Valid([]byte(bodyCopy)) {
			return nil, domain.ErrInvalidIdempotencyRecordData
		}
		record.ResponseBody = &bodyCopy
	}
	if record.ExpiresAt.Year() < 1000 || record.ExpiresAt.Year() > 9999 || !record.ExpiresAt.After(time.Now()) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 3. 插入幂等结果，处理取消、重复键和数据库故障。
	result := store.database.WithContext(writeContext).
		Select("user_id", "operation", "idempotency_key", "request_hash", "resource_type", "resource_id", "response_status", "response_body", "expires_at").Create(&record)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		var databaseError *mysql.MySQLError
		if errors.As(result.Error, &databaseError) && databaseError.Number == 1062 {
			return nil, domain.ErrIdempotencyRecordAlreadyExists
		}
		return nil, fmt.Errorf("保存幂等结果失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}

	// 4. 读回并核对记录，交由外层事务确认提交。
	saved, err := store.FindUserIdempotencyRecord(writeContext, actor.UserID, record.Operation, record.IdempotencyKey)
	if errors.Is(err, domain.ErrIdempotencyRecordNotFound) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	if err != nil {
		return nil, fmt.Errorf("读取已保存幂等结果失败: %w", err)
	}
	if saved.RequestHash != input.RequestHash || saved.ResourceType != record.ResourceType || saved.ResponseStatus != record.ResponseStatus ||
		!saved.ExpiresAt.Equal(record.ExpiresAt) || (saved.ResourceID == nil) != (record.ResourceID == nil) ||
		(record.ResourceID != nil && *saved.ResourceID != *record.ResourceID) || (saved.ResponseBody == nil) != (record.ResponseBody == nil) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return saved, nil
}
