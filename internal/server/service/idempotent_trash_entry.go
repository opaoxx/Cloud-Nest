package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// IdempotentTrashEntryTransaction 提供同一事务内的回收和幂等记录操作。
type IdempotentTrashEntryTransaction interface {
	TrashEntryTransaction
	// FindUserIdempotencyRecord 按用户、操作和键读取稳定结果。
	FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error)
	// CreateUserIdempotencyRecord 保存结果并读回，外层事务决定提交或回滚。
	CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error)
}

// IdempotentTrashEntryService 组合首次回收和同键结果复用。
type IdempotentTrashEntryService struct {
	// withTransaction 同步执行一次回调，确认提交后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentTrashEntryTransaction) error) error
	// encodeResult 交由 DTO 将回收记录编码为稳定 JSON。
	encodeResult func(*domain.TrashedFileEntry) (json.RawMessage, error)
	// validateResult 复用快照恢复校验，保留正文原值。
	validateResult func(json.RawMessage) error
	// now 提供期限和复用判断使用的应用时间。
	now func() time.Time
}

// NewIdempotentTrashEntryService 绑定事务入口和稳定结果编解码能力。
func NewIdempotentTrashEntryService(
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentTrashEntryTransaction) error) error,
	encodeResult func(*domain.TrashedFileEntry) (json.RawMessage, error),
	validateResult func(json.RawMessage) error,
) *IdempotentTrashEntryService {
	// 1. 检查装配依赖，保存事务、结果编码与完整校验入口。
	if withTransaction == nil || encodeResult == nil || validateResult == nil {
		panic("幂等回收服务需要事务、结果编码和校验方法")
	}
	return &IdempotentTrashEntryService{
		withTransaction: withTransaction, encodeResult: encodeResult, validateResult: validateResult, now: time.Now,
	}
}

// Trash 在同一认证用户事务内保存首次结果或复用同键同参数快照。
func (trashService *IdempotentTrashEntryService) Trash(ctx context.Context, actor domain.Actor, entryID, expectedRevision uint64, key string) (json.RawMessage, error) {
	// 1. 校验调用参数，生成请求摘要。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !domain.ValidIdempotencyKey(key) {
		return nil, fmt.Errorf("幂等回收需要上下文、完整认证身份和有效幂等键")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requestHash, err := BuildTrashEntryRequestHash(entryID, expectedRevision)
	if err != nil {
		return nil, err
	}

	// 2. 进入已认证用户事务，查找当前幂等记录。
	var prepared json.RawMessage
	transactionError := trashService.withTransaction(ctx, actor, func(txContext context.Context, txStore IdempotentTrashEntryTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("幂等回收回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		record, findError := txStore.FindUserIdempotencyRecord(txContext, actor.UserID, domain.TrashEntryOperation, key)
		if err := txContext.Err(); err != nil {
			return err
		}
		if findError != nil && !errors.Is(findError, domain.ErrIdempotencyRecordNotFound) {
			return fmt.Errorf("查询回收幂等结果失败: %w", findError)
		}

		// 3. 首次请求执行回收，编码并保存结果。
		if errors.Is(findError, domain.ErrIdempotencyRecordNotFound) {
			if record != nil {
				return domain.ErrInvalidIdempotencyRecordData
			}
			// 内层回收复用当前事务，最外层负责确认提交。
			core := NewTrashEntryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, TrashEntryTransaction) error) error {
				return action(ctx, txStore)
			})
			entry, err := core.Trash(txContext, actor, entryID, expectedRevision)
			if err != nil {
				return err
			}
			resourceID := entry.ID
			body, encodeError := trashService.encodeResult(entry)
			if err := txContext.Err(); err != nil {
				return err
			}
			if encodeError != nil {
				return fmt.Errorf("编码回收结果失败: %w", encodeError)
			}
			if err := trashService.validateResult(body); err != nil {
				return err
			}
			if err := txContext.Err(); err != nil {
				return err
			}
			resultTime := trashService.now().UTC().Truncate(time.Microsecond)
			if resultTime.IsZero() {
				return domain.ErrInvalidIdempotencyRecordData
			}
			record, err = txStore.CreateUserIdempotencyRecord(txContext, domain.IdempotencyResultInput{
				Operation: domain.TrashEntryOperation, IdempotencyKey: key, RequestHash: requestHash,
				ResourceType: "ENTRY", ResourceID: &resourceID, ResponseStatus: 200, ResponseBody: bytes.Clone(body),
				ExpiresAt: resultTime.Add(8 * 24 * time.Hour),
			})
			if err := txContext.Err(); err != nil {
				return err
			}
			if err != nil {
				return fmt.Errorf("保存回收幂等结果失败: %w", err)
			}
		}

		// 4. 校验当前记录的复用条件与完整正文，准备独立快照。
		if err := ValidateTrashEntryReplay(record, actor.UserID, entryID, expectedRevision, key, requestHash, trashService.now()); err != nil {
			return err
		}
		if err := trashService.validateResult(record.ResponseBody); err != nil {
			return err
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		prepared = bytes.Clone(record.ResponseBody)
		return nil
	})

	// 5. 确认外层事务提交后交付快照。
	if transactionError != nil {
		return nil, fmt.Errorf("幂等回收事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return prepared, nil
}
