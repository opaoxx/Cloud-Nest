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

// IdempotentFileReuseTransaction 汇集秒传、幂等结果和发布事件使用的同一账号事务能力。
type IdempotentFileReuseTransaction interface {
	FileReuseTransaction
	FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error)
	CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error)
	AppendUserChangeEvent(ctx context.Context, input domain.ChangeEventInput) (uint64, error)
}

// IdempotentFileReuseService 保存首次秒传结果，并在同一账号范围重放有效快照。
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

// Reuse 首次执行秒传或重放有效的同键同参结果，事务成功后返回公开快照。
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

		// 2. 先读幂等记录，只在缺失时进入首次写入流程。
		record, readError := txn.FindUserIdempotencyRecord(txContext, actor.UserID, domain.FileReuseOperation, key)
		if err := txContext.Err(); err != nil {
			return err
		}
		if readError != nil {
			if !errors.Is(readError, domain.ErrIdempotencyRecordNotFound) {
				return readError
			}
			if record != nil {
				return domain.ErrInvalidIdempotencyRecordData
			}
			// 3. 在当前事务创建并结算新文件，再保存快照与发布事件。
			creator := NewFileReuseService(func(ctx context.Context, _ domain.Actor, action func(context.Context, FileReuseTransaction) error) error {
				return action(ctx, txn)
			})
			entry, err := creator.Reuse(txContext, actor, parentID, requestedName, sizeBytes, digest)
			if err != nil {
				return err
			}
			record, err = reuse.saveFirstResult(txContext, txn, entry, key, requestHash)
			if err != nil {
				return err
			}
			event, err := newFilePublishedChangeEvent(entry, digest)
			if err != nil {
				return fmt.Errorf("编码秒传发布事件失败: %w", err)
			}
			if _, err := txn.AppendUserChangeEvent(txContext, event); err != nil {
				return fmt.Errorf("追加秒传发布事件失败: %w", err)
			}
		}

		// 4. 核对记录范围、期限、参数指纹及公开快照，准备原创建结果。
		if err := validateFileReuseReplay(record, actor.UserID, parentID, sizeBytes, key, requestHash, time.Now()); err != nil {
			return err
		}
		if err := reuse.validateResult(record.ResponseBody); err != nil {
			return err
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		prepared = bytes.Clone(record.ResponseBody)
		return nil
	})

	// 5. 确认提交后交付结果，任一步报错则整体回滚。
	if err != nil {
		return nil, err
	}
	if prepared == nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return prepared, nil
}

// saveFirstResult 编码并保存首次创建快照，外层继续校验读回结果。
func (reuse *IdempotentFileReuseService) saveFirstResult(
	ctx context.Context, txn IdempotentFileReuseTransaction, entry *domain.FileEntry, key string, requestHash [sha256.Size]byte,
) (*domain.IdempotencyRecord, error) {
	// 1. 编码公开快照并校验字段。
	body, err := reuse.encodeResult(entry)
	if err != nil {
		return nil, err
	}
	if err := reuse.validateResult(body); err != nil {
		return nil, err
	}

	// 2. 保存条目关联和八天期限，核对写入结果。
	resourceID := entry.ID
	expiresAt := entry.CreatedAt.UTC().Truncate(time.Microsecond).Add(fileReuseResultRetention)
	record, err := txn.CreateUserIdempotencyRecord(ctx, domain.IdempotencyResultInput{
		Operation: domain.FileReuseOperation, IdempotencyKey: key, RequestHash: requestHash,
		ResourceType: "ENTRY", ResourceID: &resourceID, ResponseStatus: http.StatusCreated,
		ResponseBody: bytes.Clone(body), ExpiresAt: expiresAt,
	})
	if err != nil {
		return nil, err
	}
	if record == nil || record.ResourceID == nil || *record.ResourceID != entry.ID || !record.ExpiresAt.Equal(expiresAt) {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return record, nil
}

// validateFileReuseReplay 确认记录在保留期内并关联本次秒传的公开快照。
func validateFileReuseReplay(
	record *domain.IdempotencyRecord, userID, parentID, sizeBytes uint64, key string, requestHash [sha256.Size]byte, now time.Time,
) error {
	// 1. 复用目录项操作的范围、期限和指纹校验。
	if err := validateEntryReplayRecord(record, userID, domain.FileReuseOperation, key, requestHash, now); err != nil {
		return err
	}

	// 2. 核对 201 记录及快照中的条目关联，完整字段由快照解码器核验。
	if record.ResourceType != "ENTRY" || record.ResourceID == nil || *record.ResourceID == 0 || record.ResponseStatus != http.StatusCreated {
		return domain.ErrInvalidIdempotencyRecordData
	}
	var snapshot struct {
		Entry struct {
			ID        string  `json:"id"`
			ParentID  string  `json:"parent_id"`
			SizeBytes *uint64 `json:"size_bytes"`
		} `json:"entry"`
	}
	if json.Unmarshal(record.ResponseBody, &snapshot) != nil || snapshot.Entry.ID != strconv.FormatUint(*record.ResourceID, 10) ||
		snapshot.Entry.ParentID != strconv.FormatUint(parentID, 10) || snapshot.Entry.SizeBytes == nil || *snapshot.Entry.SizeBytes != sizeBytes {
		return domain.ErrInvalidIdempotencyRecordData
	}
	return nil
}
