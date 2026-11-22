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

// IdempotentCreateDirectoryTransaction 汇集同一用户事务中的目录与幂等记录操作。
type IdempotentCreateDirectoryTransaction interface {
	CreateDirectoryTransaction
	// FindUserIdempotencyRecord 读取当前用户、操作与键的记录。
	FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error)
	// CreateUserIdempotencyRecord 在当前事务保存稳定结果并读回。
	CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error)
	// AppendUserChangeEvent 在当前事务内追加一次业务变更。
	AppendUserChangeEvent(ctx context.Context, input domain.ChangeEventInput) (uint64, error)
}

// IdempotentCreateDirectoryService 组合首次创建和同键结果复用。
type IdempotentCreateDirectoryService struct {
	// withTransaction 同步执行一次回调，传播回调错误，仅在确认提交后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentCreateDirectoryTransaction) error) error
	// encodeResult 复用创建 DTO，将目录和颜色编码为独立稳定 JSON。
	encodeResult func(*domain.FileEntry, *string) (json.RawMessage, error)
	// validateResult 复用创建快照恢复方法，核对完整字段并保留输入原值。
	validateResult func(json.RawMessage) error
	// now 提供期限与复用判断时间，默认使用真实时钟。
	now func() time.Time
}

// NewIdempotentCreateDirectoryService 绑定事务入口与创建结果编解码能力。
func NewIdempotentCreateDirectoryService(
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentCreateDirectoryTransaction) error) error,
	encodeResult func(*domain.FileEntry, *string) (json.RawMessage, error),
	validateResult func(json.RawMessage) error,
) *IdempotentCreateDirectoryService {
	// 1. 检查装配依赖，保存事务与结果转换方法。
	if withTransaction == nil || encodeResult == nil || validateResult == nil {
		panic("幂等创建文件夹服务需要事务、结果编码和校验方法")
	}
	return &IdempotentCreateDirectoryService{
		withTransaction: withTransaction, encodeResult: encodeResult, validateResult: validateResult, now: time.Now,
	}
}

// Create 在同一用户事务中创建并保存结果，或复用同键同参数的已存结果。
func (createService *IdempotentCreateDirectoryService) Create(ctx context.Context, actor domain.Actor, parentID uint64, rawName string, colorKey *string, key string) (json.RawMessage, error) {
	// 1. 检查身份、请求键及取消，复制颜色并生成请求摘要。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !domain.ValidIdempotencyKey(key) {
		return nil, fmt.Errorf("幂等创建文件夹需要上下文、完整认证身份和有效幂等键")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	colorCopy := copyOptionalValue(colorKey)
	requestHash, err := BuildCreateDirectoryRequestHash(parentID, rawName, colorCopy)
	if err != nil {
		return nil, err
	}

	// 2. 进入用户事务，读取当前请求的幂等记录。
	var prepared json.RawMessage
	transactionError := createService.withTransaction(ctx, actor, func(txContext context.Context, txStore IdempotentCreateDirectoryTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("幂等创建文件夹回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		record, findError := txStore.FindUserIdempotencyRecord(txContext, actor.UserID, domain.CreateDirectoryOperation, key)
		if err := txContext.Err(); err != nil {
			return err
		}
		if findError != nil && !errors.Is(findError, domain.ErrIdempotencyRecordNotFound) {
			return fmt.Errorf("查询创建文件夹幂等结果失败: %w", findError)
		}

		// 3. 记录缺失时，复用当前事务完成目录创建。
		if errors.Is(findError, domain.ErrIdempotencyRecordNotFound) {
			if record != nil {
				return domain.ErrInvalidIdempotencyRecordData
			}
			// 核心创建流程使用当前事务，最终提交由最外层事务入口负责。
			creator := NewCreateDirectoryService(func(ctx context.Context, current domain.Actor, action func(context.Context, CreateDirectoryTransaction) error) error {
				return action(ctx, txStore)
			})
			entry, err := creator.Create(txContext, actor, parentID, rawName, colorCopy)
			if err != nil {
				return err
			}

			// 4. 编码稳定结果并与目录一起保存。
			resourceID := entry.ID
			body, err := createService.encodeResult(entry, copyOptionalValue(colorCopy))
			if err != nil {
				return fmt.Errorf("编码创建文件夹结果失败: %w", err)
			}
			if err := createService.validateResult(body); err != nil {
				return err
			}
			if err := txContext.Err(); err != nil {
				return err
			}
			resultTime := createService.now().UTC().Truncate(time.Microsecond)
			if resultTime.IsZero() {
				return domain.ErrInvalidIdempotencyRecordData
			}
			record, err = txStore.CreateUserIdempotencyRecord(txContext, domain.IdempotencyResultInput{
				Operation: domain.CreateDirectoryOperation, IdempotencyKey: key, RequestHash: requestHash,
				ResourceType: "ENTRY", ResourceID: &resourceID, ResponseStatus: 201, ResponseBody: bytes.Clone(body),
				ExpiresAt: resultTime.Add(8 * 24 * time.Hour),
			})
			if err := txContext.Err(); err != nil {
				return err
			}
			if err != nil {
				return fmt.Errorf("保存创建文件夹幂等结果失败: %w", err)
			}

			// 5. 从已核对的新目录构造事件，和目录、幂等快照一同提交。
			payload, err := json.Marshal(struct {
				EntryID  string `json:"entry_id"`
				ParentID string `json:"parent_id"`
				Name     string `json:"name"`
				Revision string `json:"revision"`
			}{
				EntryID: strconv.FormatUint(entry.ID, 10), ParentID: strconv.FormatUint(*entry.ParentID, 10),
				Name: entry.Name, Revision: strconv.FormatUint(entry.Revision, 10),
			})
			if err != nil {
				return fmt.Errorf("编码创建文件夹变更事件失败: %w", err)
			}
			if _, err := txStore.AppendUserChangeEvent(txContext, domain.ChangeEventInput{
				EntryID: entry.ID, Revision: entry.Revision, EventType: "DIRECTORY_CREATED", Payload: payload,
			}); err != nil {
				return fmt.Errorf("追加创建文件夹变更事件失败: %w", err)
			}
		}

		// 6. 首次创建和重放都校验完整快照，准备独立正文。
		if err := ValidateCreateDirectoryReplay(record, actor.UserID, key, requestHash, createService.now()); err != nil {
			return err
		}
		if err := createService.validateResult(record.ResponseBody); err != nil {
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
		return nil, fmt.Errorf("幂等创建文件夹事务失败: %w", transactionError)
	}
	if prepared == nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return prepared, nil
}
