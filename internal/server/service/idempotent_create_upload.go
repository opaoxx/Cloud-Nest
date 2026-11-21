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

// IdempotentCreateUploadTransaction 汇集同一用户事务内的核心创建与幂等记录能力。
type IdempotentCreateUploadTransaction interface {
	CreateUploadTransaction
	// FindUserIdempotencyRecord 读取当前用户、操作及键范围的结果。
	FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error)
	// CreateUserIdempotencyRecord 保存稳定结果，最终由外层事务确认。
	CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error)
}

// IdempotentCreateUploadService 组合首次上传创建和同键结果复用。
type IdempotentCreateUploadService struct {
	// withTransaction 同步执行用户事务，传播错误并确认提交。
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentCreateUploadTransaction) error) error
	// provider 保存已校验的服务端存储逻辑标识。
	provider string
	// bucket 保存已校验的服务端存储桶名称。
	bucket string
	// encodeResult 绑定创建 DTO 编码方法，生成稳定 JSON。
	encodeResult func(*domain.UploadTask) (json.RawMessage, error)
	// validateResult 绑定完整创建快照恢复方法，保留正文原值。
	validateResult func(json.RawMessage) error
	// now 提供结果保留及复用判断时间，默认使用真实时钟。
	now func() time.Time
}

// NewIdempotentCreateUploadService 绑定已校验存储配置、用户事务和结果编解码。
func NewIdempotentCreateUploadService(provider, bucket string,
	withTransaction func(context.Context, domain.Actor, func(context.Context, IdempotentCreateUploadTransaction) error) error,
	encodeResult func(*domain.UploadTask) (json.RawMessage, error),
	validateResult func(json.RawMessage) error,
) *IdempotentCreateUploadService {
	// 1. 检查依赖与位置列约束，保存装配参数。
	location := domain.CreateStagedObjectInput{Provider: provider, Bucket: bucket, ObjectKey: "validation"}
	if withTransaction == nil || encodeResult == nil || validateResult == nil || !location.IsValid() {
		panic("幂等上传创建服务需要存储配置、用户事务和结果编解码方法")
	}
	return &IdempotentCreateUploadService{
		withTransaction: withTransaction,
		provider:        provider,
		bucket:          bucket,
		encodeResult:    encodeResult,
		validateResult:  validateResult,
		now:             time.Now,
	}
}

// Create 在一次用户事务中创建 SINGLE 上传，或复用同键同参数的已存结果。
func (createService *IdempotentCreateUploadService) Create(ctx context.Context, actor domain.Actor, input domain.CreateUploadInput, key string) (json.RawMessage, error) {
	return createService.create(ctx, actor, input, key, nil)
}

// CreateWithPlan 支持两种上传模式，仅在首次创建时应用当前限制。
// 装配的编码器和恢复校验器须支持两种创建快照。
func (createService *IdempotentCreateUploadService) CreateWithPlan(ctx context.Context, actor domain.Actor, input domain.CreateUploadInput, key string, limits domain.UploadPlanLimits) (json.RawMessage, error) {
	return createService.create(ctx, actor, input, key, &limits)
}

func (createService *IdempotentCreateUploadService) create(ctx context.Context, actor domain.Actor, input domain.CreateUploadInput, key string, limits *domain.UploadPlanLimits) (json.RawMessage, error) {
	// 1. 校验身份、请求键和声明，复制输入并生成摘要。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || !domain.ValidIdempotencyKey(key) {
		return nil, fmt.Errorf("幂等上传创建需要上下文、完整认证身份和有效幂等键")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input.ExpectedSHA256 = copyOptionalValue(input.ExpectedSHA256)
	buildHash := BuildCreateUploadRequestHash
	expectedMode := ""
	if limits != nil {
		buildHash = BuildCreateUploadRequestHashWithMultipart
		expectedMode = "SINGLE"
		if input.SizeBytes > domain.MaxSingleUploadSizeBytes {
			expectedMode = "MULTIPART"
		}
	}
	requestHash, err := buildHash(input)
	if err != nil {
		return nil, err
	}

	// 2. 进入用户事务，查找当前请求的幂等记录。
	var prepared json.RawMessage
	transactionError := createService.withTransaction(ctx, actor, func(txContext context.Context, txStore IdempotentCreateUploadTransaction) error {
		if txContext == nil || txStore == nil {
			return fmt.Errorf("幂等上传创建回调需要事务上下文和 Store")
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		record, findError := txStore.FindUserIdempotencyRecord(txContext, actor.UserID, domain.CreateUploadOperation, key)
		if err := txContext.Err(); err != nil {
			return err
		}
		if findError != nil && !errors.Is(findError, domain.ErrIdempotencyRecordNotFound) {
			return fmt.Errorf("查询上传创建幂等结果失败: %w", findError)
		}

		// 3. 记录缺失才执行核心创建，分片计划在此时按当前限制确定。
		if errors.Is(findError, domain.ErrIdempotencyRecordNotFound) {
			if record != nil {
				return domain.ErrInvalidIdempotencyRecordData
			}
			creator := NewCreateUploadService(createService.provider, createService.bucket,
				func(ctx context.Context, current domain.Actor, action func(context.Context, CreateUploadTransaction) error) error {
					return action(ctx, txStore)
				},
			)
			var task *domain.UploadTask
			var err error
			if limits == nil {
				task, err = creator.Create(txContext, actor, input)
			} else {
				task, err = creator.CreateWithPlan(txContext, actor, input, *limits)
			}
			if err != nil {
				return err
			}

			// 4. 编码并校验稳定结果，与创建数据一起保存。
			resourceID := task.ID
			body, encodeError := createService.encodeResult(task)
			if err := txContext.Err(); err != nil {
				return err
			}
			if encodeError != nil {
				return fmt.Errorf("编码上传创建结果失败: %w", encodeError)
			}
			validationError := createService.validateSnapshot(body, expectedMode)
			if err := txContext.Err(); err != nil {
				return err
			}
			if validationError != nil {
				return validationError
			}
			resultTime := createService.now().UTC().Truncate(time.Microsecond)
			if resultTime.IsZero() {
				return domain.ErrInvalidIdempotencyRecordData
			}
			record, err = txStore.CreateUserIdempotencyRecord(txContext, domain.IdempotencyResultInput{
				Operation:      domain.CreateUploadOperation,
				IdempotencyKey: key,
				RequestHash:    requestHash,
				ResourceType:   "UPLOAD_TASK",
				ResourceID:     &resourceID,
				ResponseStatus: 201,
				ResponseBody:   bytes.Clone(body),
				ExpiresAt:      resultTime.Add(8 * 24 * time.Hour),
			})
			if err := txContext.Err(); err != nil {
				return err
			}
			if err != nil {
				return fmt.Errorf("保存上传创建幂等结果失败: %w", err)
			}
		}

		// 5. 校验复用条件和完整快照，复制结果正文。
		if err := ValidateCreateUploadReplay(record, actor.UserID, key, requestHash, createService.now()); err != nil {
			return err
		}
		validationError := createService.validateSnapshot(record.ResponseBody, expectedMode)
		if err := txContext.Err(); err != nil {
			return err
		}
		if validationError != nil {
			return validationError
		}
		prepared = bytes.Clone(record.ResponseBody)
		return nil
	})

	// 6. 确认外层提交成功后返回稳定 JSON。
	if transactionError != nil {
		return nil, fmt.Errorf("幂等上传创建事务失败: %w", transactionError)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if prepared == nil {
		return nil, domain.ErrInvalidIdempotencyRecordData
	}
	return prepared, nil
}

func (createService *IdempotentCreateUploadService) validateSnapshot(body json.RawMessage, expectedMode string) error {
	// 1. 两模式创建时，核对快照模式与请求摘要对应的模式一致。
	if expectedMode != "" {
		var data struct {
			UploadMode string `json:"upload_mode"`
		}
		if json.Unmarshal(body, &data) != nil || data.UploadMode != expectedMode {
			return domain.ErrInvalidIdempotencyRecordData
		}
	}
	// 2. 再由装配的校验器核对完整快照。
	return createService.validateResult(body)
}
