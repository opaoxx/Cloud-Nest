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

// IdempotentRenameEntryTransaction 汇集同一用户事务内的改名、详情及幂等记录操作。
type IdempotentRenameEntryTransaction interface {
	RenameEntryTransaction
	EntryDetailReader
	// FindUserIdempotencyRecord 读取当前用户、操作与键的记录。
	FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error)
	// CreateUserIdempotencyRecord 在当前事务保存稳定结果并读回。
	CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error)
	// AppendUserChangeEvent 在当前事务内追加一次业务变更。
	AppendUserChangeEvent(ctx context.Context, input domain.ChangeEventInput) (uint64, error)
}

// IdempotentRenameEntryService 组合首次改名和同键结果复用。
type IdempotentRenameEntryService struct {
	// withTransaction 同步执行一次回调，传播失败，仅在确认提交后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentRenameEntryTransaction) error) error
	// encodeResult 复用重命名 DTO，将事务内完整详情编码为独立稳定 JSON。
	encodeResult func(*domain.EntryDetail) (json.RawMessage, error)
	// validateResult 复用快照恢复入口，核对完整字段并保留输入原值。
	validateResult func(json.RawMessage) error
	// now 提供期限和复用判断时间，默认使用真实时钟。
	now func() time.Time
}

// NewIdempotentRenameEntryService 绑定事务入口及重命名结果编解码能力。
func NewIdempotentRenameEntryService(
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentRenameEntryTransaction) error) error,
	encodeResult func(*domain.EntryDetail) (json.RawMessage, error),
	validateResult func(json.RawMessage) error,
) *IdempotentRenameEntryService {
	// 1. 检查装配依赖，保存事务与结果转换方法。
	if withTransaction == nil || encodeResult == nil || validateResult == nil {
		panic("幂等重命名服务需要事务、结果编码和校验方法")
	}
	return &IdempotentRenameEntryService{
		withTransaction: withTransaction, encodeResult: encodeResult, validateResult: validateResult, now: time.Now,
	}
}

// Rename 在同一用户事务中改名并保存结果，或复用同键同参数的已存快照。
func (renameService *IdempotentRenameEntryService) Rename(ctx context.Context, actor domain.Actor, entryID, expectedRevision uint64, rawName, key string) (json.RawMessage, error) {
	// 1. 检查身份、幂等键和取消状态，生成请求摘要。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !domain.ValidIdempotencyKey(key) {
		return nil, fmt.Errorf("幂等重命名需要上下文、完整认证身份和有效幂等键")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requestHash, err := BuildRenameEntryRequestHash(entryID, expectedRevision, rawName)
	if err != nil {
		return nil, err
	}

	// 2. 进入用户事务，查询当前请求的幂等记录。
	var prepared json.RawMessage
	transactionError := renameService.withTransaction(ctx, actor, func(txContext context.Context, txStore IdempotentRenameEntryTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("幂等重命名回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		record, findError := txStore.FindUserIdempotencyRecord(txContext, actor.UserID, domain.RenameEntryOperation, key)
		if err := txContext.Err(); err != nil {
			return err
		}
		if findError != nil && !errors.Is(findError, domain.ErrIdempotencyRecordNotFound) {
			return fmt.Errorf("查询重命名幂等结果失败: %w", findError)
		}

		// 3. 记录缺失时，在当前事务中完成核心改名。
		if errors.Is(findError, domain.ErrIdempotencyRecordNotFound) {
			if record != nil {
				return domain.ErrInvalidIdempotencyRecordData
			}
			// 核心改名和详情读取使用当前 Store，提交由最外层用户事务负责。
			renamer := NewRenameEntryService(func(ctx context.Context, current domain.Actor, action func(context.Context, RenameEntryTransaction) error) error {
				return action(ctx, txStore)
			})
			entry, err := renamer.Rename(txContext, actor, entryID, expectedRevision, rawName)
			if err != nil {
				return err
			}

			// 4. 读取并核对完整详情，编码和保存稳定结果。
			reader := NewEntryDetailService(func(ctx context.Context, action func(context.Context, EntryDetailReader) error) error {
				return action(ctx, txStore)
			})
			detail, err := reader.Get(txContext, actor, entryID)
			if err != nil {
				return err
			}
			target := detail.Path.Entries[len(detail.Path.Entries)-1]
			if target.ParentID == nil || *target.ParentID != *entry.ParentID || target.Name != entry.Name || target.Revision != entry.Revision ||
				target.EntryType != entry.EntryType || target.FileSize != entry.FileSize || !target.CreatedAt.Equal(entry.CreatedAt) ||
				!target.ModifiedAt.Equal(entry.ModifiedAt) || !target.UpdatedAt.Equal(entry.UpdatedAt) {
				return domain.ErrInvalidFileEntryData
			}
			resourceID := entry.ID
			body, encodeError := renameService.encodeResult(detail)
			if err := txContext.Err(); err != nil {
				return err
			}
			if encodeError != nil {
				return fmt.Errorf("编码重命名结果失败: %w", encodeError)
			}
			if err := renameService.validateResult(body); err != nil {
				return err
			}
			if err := txContext.Err(); err != nil {
				return err
			}
			resultTime := renameService.now().UTC().Truncate(time.Microsecond)
			if resultTime.IsZero() {
				return domain.ErrInvalidIdempotencyRecordData
			}
			record, err = txStore.CreateUserIdempotencyRecord(txContext, domain.IdempotencyResultInput{
				Operation: domain.RenameEntryOperation, IdempotencyKey: key, RequestHash: requestHash,
				ResourceType: "ENTRY", ResourceID: &resourceID, ResponseStatus: 200, ResponseBody: bytes.Clone(body),
				ExpiresAt: resultTime.Add(8 * 24 * time.Hour),
			})
			if err := txContext.Err(); err != nil {
				return err
			}
			if err != nil {
				return fmt.Errorf("保存重命名幂等结果失败: %w", err)
			}

			// 5. 版本递增表明名称发生变化，事件和改名结果一同提交。
			if entry.Revision > expectedRevision {
				event, err := newEntryChangeEvent("ENTRY_RENAMED", entry)
				if err != nil {
					return fmt.Errorf("编码重命名变更事件失败: %w", err)
				}
				if _, err := txStore.AppendUserChangeEvent(txContext, event); err != nil {
					return fmt.Errorf("追加重命名变更事件失败: %w", err)
				}
			}
		}

		// 6. 首次重命名和重放都校验完整快照，准备独立正文。
		if err := ValidateRenameEntryReplay(record, actor.UserID, entryID, key, requestHash, renameService.now()); err != nil {
			return err
		}
		if err := renameService.validateResult(record.ResponseBody); err != nil {
			return err
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		prepared = bytes.Clone(record.ResponseBody)
		return nil
	})

	// 7. 确认外层事务提交，返回稳定结果。
	if transactionError != nil {
		return nil, fmt.Errorf("幂等重命名事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return prepared, nil
}
