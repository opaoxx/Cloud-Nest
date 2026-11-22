package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// IdempotentPurgeEntryTransaction 提供同一事务中的受理、快照及事件持久化能力。
type IdempotentPurgeEntryTransaction interface {
	PurgeEntryTransaction
	FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error)
	CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error)
	AppendUserChangeEvent(ctx context.Context, input domain.ChangeEventInput) (uint64, error)
}

// IdempotentPurgeEntryService 组合首次受理与同键结果复用。
type IdempotentPurgeEntryService struct {
	// withTransaction 同步执行一次回调，确认提交后才返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentPurgeEntryTransaction) error) error
	verifier        *PurgePreviewVerifier
	encodeResult    func(*domain.FileEntry) (json.RawMessage, error)
	decodeEntryID   func([]byte) (string, error)
	now             func() time.Time
}

// NewIdempotentPurgeEntryService 绑定用户事务、凭证校验和快照编解码。
func NewIdempotentPurgeEntryService(
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentPurgeEntryTransaction) error) error,
	verifier *PurgePreviewVerifier,
	encodeResult func(*domain.FileEntry) (json.RawMessage, error),
	decodeEntryID func([]byte) (string, error),
	now func() time.Time,
) *IdempotentPurgeEntryService {
	// 1. 检查并保存幂等受理所需的依赖。
	if withTransaction == nil || verifier == nil || encodeResult == nil || decodeEntryID == nil || now == nil {
		panic("永久清理幂等服务需要用户事务、凭证校验器、快照编解码方法和时钟")
	}
	return &IdempotentPurgeEntryService{
		withTransaction: withTransaction, verifier: verifier,
		encodeResult: encodeResult, decodeEntryID: decodeEntryID, now: now,
	}
}

// Begin 首次受理永久清理，或交付已保存的受理快照。
func (purgeService *IdempotentPurgeEntryService) Begin(ctx context.Context, actor domain.Actor, entryID, expectedRevision uint64, digest, key string) (json.RawMessage, error) {
	// 1. 校验输入并计算请求摘要。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !domain.ValidIdempotencyKey(key) {
		return nil, fmt.Errorf("永久清理幂等受理需要上下文、完整认证身份和有效幂等键")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requestHash, err := BuildPurgeEntryRequestHash(entryID, expectedRevision, digest)
	if err != nil {
		return nil, err
	}

	// 2. 在已认证用户事务内查询幂等记录。
	var prepared json.RawMessage
	transactionError := purgeService.withTransaction(ctx, actor, func(txContext context.Context, txStore IdempotentPurgeEntryTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("永久清理幂等回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		record, findError := txStore.FindUserIdempotencyRecord(txContext, actor.UserID, domain.PurgeEntryOperation, key)
		if err := txContext.Err(); err != nil {
			return err
		}
		firstRequest := errors.Is(findError, domain.ErrIdempotencyRecordNotFound)
		if findError != nil && !firstRequest {
			return fmt.Errorf("查询永久清理幂等记录失败: %w", findError)
		}

		// 3. 首次受理时写入状态并保存快照。
		if firstRequest {
			if record != nil {
				return domain.ErrInvalidIdempotencyRecordData
			}
			entry, err := beginPurgeEntryInTransaction(txContext, actor, entryID, expectedRevision, digest, txStore, purgeService.verifier, purgeService.now)
			if err != nil {
				return err
			}
			body, err := purgeService.encodeResult(entry)
			if err != nil {
				return fmt.Errorf("编码永久清理受理快照失败: %w", err)
			}
			encodedID, err := purgeService.decodeEntryID(body)
			if err != nil || encodedID != strconv.FormatUint(entryID, 10) {
				return domain.ErrInvalidIdempotencyRecordData
			}
			if err := txContext.Err(); err != nil {
				return err
			}
			resultTime := purgeService.now().UTC().Truncate(time.Microsecond)
			if resultTime.IsZero() {
				return domain.ErrInvalidIdempotencyRecordData
			}
			resourceID := entryID
			record, err = txStore.CreateUserIdempotencyRecord(txContext, domain.IdempotencyResultInput{
				Operation: domain.PurgeEntryOperation, IdempotencyKey: key, RequestHash: requestHash,
				ResourceType: "ENTRY", ResourceID: &resourceID, ResponseStatus: 202, ResponseBody: bytes.Clone(body),
				ExpiresAt: resultTime.Add(8 * 24 * time.Hour),
			})
			if err != nil {
				return fmt.Errorf("保存永久清理幂等记录失败: %w", err)
			}

			// 4. 为实际进入 PURGING 的根项追加事件，与状态、快照一起提交。
			event, err := domain.NewEntryChangeEvent(domain.EntryPurgeStartedEventType, entry)
			if err != nil {
				return fmt.Errorf("编码清理受理事件失败: %w", err)
			}
			if _, err := txStore.AppendUserChangeEvent(txContext, event); err != nil {
				return fmt.Errorf("追加清理受理事件失败: %w", err)
			}
		}

		// 5. 校验快照，事务提交后返回。
		if err := txContext.Err(); err != nil {
			return err
		}
		if err := ValidatePurgeEntryReplay(record, actor.UserID, entryID, key, requestHash, purgeService.now(), purgeService.decodeEntryID); err != nil {
			return err
		}
		prepared = bytes.Clone(record.ResponseBody)
		return txContext.Err()
	})
	if transactionError != nil {
		return nil, fmt.Errorf("永久清理幂等事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return prepared, nil
}
