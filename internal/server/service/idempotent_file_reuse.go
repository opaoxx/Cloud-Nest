package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const fileReuseResultRetention = 8 * 24 * time.Hour

// IdempotentFileReuseTransaction 汇集秒传和幂等结果使用的同一账号事务能力。
type IdempotentFileReuseTransaction interface {
	FileReuseTransaction
	FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error)
	CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error)
}

// IdempotentFileReuseService 在创建条目及结算空间的同一事务保存首次成功结果。
type IdempotentFileReuseService struct {
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentFileReuseTransaction) error) error
	encodeResult    func(*domain.FileEntry) (json.RawMessage, error)
	validateResult  func(json.RawMessage) error
}

// NewIdempotentFileReuseService 绑定认证事务和公开快照编解码。
func NewIdempotentFileReuseService(
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentFileReuseTransaction) error) error,
	encodeResult func(*domain.FileEntry) (json.RawMessage, error),
	validateResult func(json.RawMessage) error,
) *IdempotentFileReuseService {
	if withTransaction == nil || encodeResult == nil || validateResult == nil {
		panic("幂等秒传服务需要认证事务和快照编解码")
	}
	return &IdempotentFileReuseService{
		withTransaction: withTransaction, encodeResult: encodeResult, validateResult: validateResult,
	}
}

// Reuse 执行首次秒传，事务提交后返回已保存的公开快照。
// 已有幂等记录时返回 ErrIdempotencyRecordAlreadyExists。
func (reuse *IdempotentFileReuseService) Reuse(
	ctx context.Context, actor domain.Actor, parentID uint64, requestedName string, sizeBytes uint64, digest [sha256.Size]byte, key string,
) (json.RawMessage, error) {
	// 1. 校验幂等键并生成请求指纹。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !domain.ValidIdempotencyKey(key) {
		return nil, fmt.Errorf("幂等秒传需要上下文、完整认证身份和有效幂等键")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requestHash, err := BuildFileReuseRequestHash(parentID, requestedName, sizeBytes, digest)
	if err != nil {
		return nil, err
	}

	var prepared json.RawMessage
	err = reuse.withTransaction(ctx, actor, func(txContext context.Context, txn IdempotentFileReuseTransaction) error {
		if txContext == nil || txn == nil {
			return fmt.Errorf("幂等秒传回调需要事务上下文和仓储能力")
		}

		// 2. 在账号事务内确认首次执行，复用条目创建和空间结算。
		existing, err := txn.FindUserIdempotencyRecord(txContext, actor.UserID, domain.FileReuseOperation, key)
		if err == nil && existing != nil {
			return domain.ErrIdempotencyRecordAlreadyExists
		}
		if err != nil && !errors.Is(err, domain.ErrIdempotencyRecordNotFound) {
			return err
		}
		if existing != nil || err == nil {
			return domain.ErrInvalidIdempotencyRecordData
		}
		// 把核心服务回调交给当前事务，提交由最外层账号事务统一负责。
		creator := NewFileReuseService(func(ctx context.Context, _ domain.Actor, action func(context.Context, FileReuseTransaction) error) error {
			return action(ctx, txn)
		})
		entry, err := creator.Reuse(txContext, actor, parentID, requestedName, sizeBytes, digest)
		if err != nil {
			return err
		}

		// 3. 编码并保存成功快照，读回校验。
		body, err := reuse.encodeResult(entry)
		if err != nil {
			return err
		}
		if err := reuse.validateResult(body); err != nil {
			return err
		}
		resourceID := entry.ID
		expiresAt := entry.CreatedAt.UTC().Truncate(time.Microsecond).Add(fileReuseResultRetention)
		record, err := txn.CreateUserIdempotencyRecord(txContext, domain.IdempotencyResultInput{
			Operation: domain.FileReuseOperation, IdempotencyKey: key, RequestHash: requestHash,
			ResourceType: "ENTRY", ResourceID: &resourceID, ResponseStatus: http.StatusCreated,
			ResponseBody: bytes.Clone(body), ExpiresAt: expiresAt,
		})
		if err != nil {
			return err
		}
		if record == nil || record.ID == 0 || record.UserID != actor.UserID || record.Operation != domain.FileReuseOperation ||
			record.IdempotencyKey != key || record.RequestHash != requestHash || record.ResourceType != "ENTRY" ||
			record.ResourceID == nil || *record.ResourceID != resourceID || record.ResponseStatus != http.StatusCreated ||
			!record.ExpiresAt.Equal(expiresAt) {
			return domain.ErrInvalidIdempotencyRecordData
		}
		if err := reuse.validateResult(record.ResponseBody); err != nil {
			return err
		}
		var snapshot struct {
			Entry struct {
				ID string `json:"id"`
			} `json:"entry"`
		}
		if json.Unmarshal(record.ResponseBody, &snapshot) != nil || snapshot.Entry.ID != strconv.FormatUint(resourceID, 10) {
			return domain.ErrInvalidIdempotencyRecordData
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		prepared = bytes.Clone(record.ResponseBody)
		return nil
	})

	// 4. 确认提交后交付结果，任一步报错则整体回滚。
	if err != nil {
		return nil, err
	}
	if prepared == nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return prepared, nil
}
