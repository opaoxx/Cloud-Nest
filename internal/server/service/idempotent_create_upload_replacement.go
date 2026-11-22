package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const uploadReplacementReceiptRetention = 8 * 24 * time.Hour

// IdempotentCreateUploadReplacementTransaction 汇集同一账号事务内的替换创建和幂等回执能力。
type IdempotentCreateUploadReplacementTransaction interface {
	CreateUploadReplacementTransaction
	FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error)
	CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error)
}

// IdempotentCreateUploadReplacementService 组合首次创建和同键重试。
type IdempotentCreateUploadReplacementService struct {
	withTransaction  func(context.Context, domain.Actor, func(context.Context, IdempotentCreateUploadReplacementTransaction) error) error
	provider         string
	bucket           string
	encodeSnapshot   func(*domain.UploadTask) (json.RawMessage, error)
	validateSnapshot func(body []byte, taskID, entryID, expectedRevision, sizeBytes uint64) error
}

// NewIdempotentCreateUploadReplacementService 绑定已认证账号事务、暂存位置和快照编解码。
func NewIdempotentCreateUploadReplacementService(provider, bucket string,
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentCreateUploadReplacementTransaction) error) error,
	encodeSnapshot func(*domain.UploadTask) (json.RawMessage, error),
	validateSnapshot func(body []byte, taskID, entryID, expectedRevision, sizeBytes uint64) error,
) *IdempotentCreateUploadReplacementService {
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if withTransaction == nil || encodeSnapshot == nil || validateSnapshot == nil || !location.IsValid() {
		panic("内容替换幂等创建需要账号事务、有效暂存位置和快照编解码")
	}
	return &IdempotentCreateUploadReplacementService{
		withTransaction: withTransaction, provider: provider, bucket: bucket,
		encodeSnapshot: encodeSnapshot, validateSnapshot: validateSnapshot,
	}
}

// Create 在同一账号事务中创建替换任务并保存回执，或交付已经保存的创建快照。
func (creator *IdempotentCreateUploadReplacementService) Create(ctx context.Context, actor domain.Actor,
	declaration UploadReplacementDeclaration, key string, limits domain.UploadPlanLimits,
) (json.RawMessage, error) {
	// 1. 校验身份和幂等键，计算请求摘要。
	if err := validateUploadCreationContext(ctx, actor); err != nil {
		return nil, err
	}
	if !domain.ValidIdempotencyKey(key) {
		return nil, fmt.Errorf("创建内容替换任务需要有效的幂等键")
	}
	requestHash, err := BuildCreateUploadReplacementRequestHash(declaration.EntryID, declaration.ExpectedRevision, declaration.SizeBytes, declaration.ExpectedSHA256)
	if err != nil {
		return nil, err
	}

	// 2. 在账号事务内查找回执，首次请求才创建并保存。
	var snapshot json.RawMessage
	transactionError := creator.withTransaction(ctx, actor, func(txContext context.Context, transaction IdempotentCreateUploadReplacementTransaction) error {
		if txContext == nil || transaction == nil {
			return fmt.Errorf("内容替换创建回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		record, findError := transaction.FindUserIdempotencyRecord(txContext, actor.UserID, domain.CreateUploadReplacementOperation, key)
		if err := txContext.Err(); err != nil {
			return err
		}
		if errors.Is(findError, domain.ErrIdempotencyRecordNotFound) {
			if record != nil {
				return domain.ErrInvalidIdempotencyRecordData
			}
			var saveError error
			record, saveError = creator.createAndSaveReceipt(txContext, actor, declaration, key, requestHash, limits, transaction)
			if saveError != nil {
				return saveError
			}
		} else if findError != nil {
			return fmt.Errorf("查询内容替换创建回执失败: %w", findError)
		}

		// 3. 核对保存结果，事务提交后返回快照。
		if err := ValidateCreateUploadReplacementReplay(record, actor.UserID, declaration, key, requestHash, time.Now(), creator.validateSnapshot); err != nil {
			return err
		}
		snapshot = bytes.Clone(record.ResponseBody)
		return txContext.Err()
	})
	if transactionError != nil {
		return nil, fmt.Errorf("内容替换幂等创建事务失败: %w", transactionError)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return snapshot, nil
}

func (creator *IdempotentCreateUploadReplacementService) createAndSaveReceipt(ctx context.Context, actor domain.Actor,
	declaration UploadReplacementDeclaration, key string, requestHash [sha256.Size]byte, limits domain.UploadPlanLimits,
	transaction IdempotentCreateUploadReplacementTransaction,
) (*domain.IdempotencyRecord, error) {
	// 1. 执行首次创建，在原账号事务内预占、登记暂存对象并写入任务。
	task, err := CreateUploadReplacementInTransaction(ctx, actor, declaration, limits, creator.provider, creator.bucket, transaction)
	if err != nil {
		return nil, err
	}

	// 2. 编码并校验首次 201 快照，确认它与刚创建的任务一致。
	body, err := creator.encodeSnapshot(task)
	if err != nil {
		return nil, fmt.Errorf("编码内容替换创建快照失败: %w", err)
	}
	resourceID := task.ID
	if err := creator.validateSnapshot(body, resourceID, declaration.EntryID, declaration.ExpectedRevision, declaration.SizeBytes); err != nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 3. 以同一事务保存八天回执，核对读回的任务标识和保留期限。
	expiresAt := time.Now().UTC().Truncate(time.Microsecond).Add(uploadReplacementReceiptRetention)
	record, err := transaction.CreateUserIdempotencyRecord(ctx, domain.IdempotencyResultInput{
		Operation: domain.CreateUploadReplacementOperation, IdempotencyKey: key, RequestHash: requestHash,
		ResourceType: "UPLOAD_TASK", ResourceID: &resourceID, ResponseStatus: 201,
		ResponseBody: bytes.Clone(body), ExpiresAt: expiresAt,
	})
	if err != nil {
		return nil, fmt.Errorf("保存内容替换创建回执失败: %w", err)
	}
	if record == nil || record.ResourceID == nil || *record.ResourceID != resourceID || !record.ExpiresAt.Equal(expiresAt) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return record, nil
}
