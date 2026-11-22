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

// IdempotentRestoreEntryTransaction 提供同一事务中的恢复和幂等记录操作。
type IdempotentRestoreEntryTransaction interface {
	RestoreEntryTransaction
	// FindUserIdempotencyRecord 按用户、操作和键读取稳定结果。
	FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error)
	// CreateUserIdempotencyRecord 保存结果并读回，外层事务决定提交或回滚。
	CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error)
	// AppendUserChangeEvent 在当前事务内追加一次业务变更。
	AppendUserChangeEvent(ctx context.Context, input domain.ChangeEventInput) (uint64, error)
}

// IdempotentRestoreEntryService 组合首次恢复和同键结果复用。
type IdempotentRestoreEntryService struct {
	// withTransaction 同步执行一次回调，确认提交后才返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentRestoreEntryTransaction) error) error
	// encodeResult 将本次恢复结果转换为稳定 JSON。
	encodeResult func(*domain.FileEntry) (json.RawMessage, error)
	// validateResult 通过快照解码器校验完整公开字段。
	validateResult func(json.RawMessage) error
	// now 提供保留期限和复用判断使用的应用时间。
	now func() time.Time
}

// NewIdempotentRestoreEntryService 绑定认证用户事务和快照编解码能力。
func NewIdempotentRestoreEntryService(
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentRestoreEntryTransaction) error) error,
	encodeResult func(*domain.FileEntry) (json.RawMessage, error),
	validateResult func(json.RawMessage) error,
) *IdempotentRestoreEntryService {
	// 1. 检查装配依赖，保存事务入口和快照转换方法。
	if withTransaction == nil || encodeResult == nil || validateResult == nil {
		panic("幂等恢复服务需要事务、结果编码和校验方法")
	}
	return &IdempotentRestoreEntryService{
		withTransaction: withTransaction, encodeResult: encodeResult, validateResult: validateResult, now: time.Now,
	}
}

// Restore 保存首次恢复结果，或在提交后交付同键同参数的独立快照。
func (restoreService *IdempotentRestoreEntryService) Restore(ctx context.Context, actor domain.Actor, entryID, expectedRevision uint64, key string) (json.RawMessage, error) {
	// 1. 校验输入，为目标 ID 和预期版本生成请求摘要。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !domain.ValidIdempotencyKey(key) {
		return nil, fmt.Errorf("幂等恢复需要上下文、完整认证身份和有效幂等键")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requestHash, err := BuildRestoreEntryRequestHash(entryID, expectedRevision)
	if err != nil {
		return nil, err
	}

	// 2. 在已认证用户事务中，按用户、恢复操作和幂等键查询记录。
	var prepared json.RawMessage
	transactionError := restoreService.withTransaction(ctx, actor, func(txContext context.Context, txStore IdempotentRestoreEntryTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("幂等恢复回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		record, findError := txStore.FindUserIdempotencyRecord(txContext, actor.UserID, domain.RestoreEntryOperation, key)
		if err := txContext.Err(); err != nil {
			return err
		}
		needsRestore := errors.Is(findError, domain.ErrIdempotencyRecordNotFound)
		if findError != nil && !needsRestore {
			return fmt.Errorf("查询恢复幂等结果失败: %w", findError)
		}

		// 3. 首次请求执行恢复，在同一事务保存八天有效的快照。
		if needsRestore {
			if record != nil {
				return domain.ErrInvalidIdempotencyRecordData
			}
			// 核心服务复用当前 Store，实际提交由外层事务负责。
			core := NewRestoreEntryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, RestoreEntryTransaction) error) error {
				return action(ctx, txStore)
			})
			entry, err := core.Restore(txContext, actor, entryID, expectedRevision)
			if err != nil {
				return err
			}
			body, encodeError := restoreService.encodeResult(entry)
			if err := txContext.Err(); err != nil {
				return err
			}
			if encodeError != nil {
				return fmt.Errorf("编码恢复结果失败: %w", encodeError)
			}
			if err := restoreService.validateResult(body); err != nil {
				return err
			}
			if err := txContext.Err(); err != nil {
				return err
			}
			resultTime := restoreService.now().UTC().Truncate(time.Microsecond)
			if resultTime.IsZero() {
				return domain.ErrInvalidIdempotencyRecordData
			}
			resourceID := entry.ID
			record, err = txStore.CreateUserIdempotencyRecord(txContext, domain.IdempotencyResultInput{
				Operation: domain.RestoreEntryOperation, IdempotencyKey: key, RequestHash: requestHash,
				ResourceType: "ENTRY", ResourceID: &resourceID, ResponseStatus: 200, ResponseBody: bytes.Clone(body),
				ExpiresAt: resultTime.Add(8 * 24 * time.Hour),
			})
			if err := txContext.Err(); err != nil {
				return err
			}
			if err != nil {
				return fmt.Errorf("保存恢复幂等结果失败: %w", err)
			}

			// 4. 事件使用实际恢复位置和最终名称，与恢复和快照一起提交。
			event, err := domain.NewEntryChangeEvent("ENTRY_RESTORED", entry)
			if err != nil {
				return fmt.Errorf("编码恢复变更事件失败: %w", err)
			}
			if _, err := txStore.AppendUserChangeEvent(txContext, event); err != nil {
				return fmt.Errorf("追加恢复变更事件失败: %w", err)
			}
		}

		// 5. 校验快照和本次请求的对应关系。
		if err := ValidateRestoreEntryReplay(record, actor.UserID, entryID, expectedRevision, key, requestHash, restoreService.now()); err != nil {
			return err
		}
		if err := restoreService.validateResult(record.ResponseBody); err != nil {
			return err
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		prepared = bytes.Clone(record.ResponseBody)
		return nil
	})

	// 6. 确认外层事务提交后交付快照。
	if transactionError != nil {
		return nil, fmt.Errorf("幂等恢复事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return prepared, nil
}
