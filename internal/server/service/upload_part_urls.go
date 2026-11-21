package service

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadPartURL 保存客户端上传一片所需的签名和校验参数。
type UploadPartURL struct {
	PartNumber     uint32
	SizeBytes      uint64
	ChecksumSHA256 string
	URL            string
	URLExpiresAt   time.Time
}

// UploadPartURLBatch 保存当前任务的期限和按请求顺序签发的链接。
type UploadPartURLBatch struct {
	ExpiresAt time.Time
	Parts     []UploadPartURL
}

// UploadPartURLService 组合签名目标读取、逐片签名和交付前复查。
type UploadPartURLService struct {
	getTarget func(context.Context, domain.Actor, uint64) (*UploadPartSigningTarget, error)
	signPart  func(context.Context, string, string, uint32, uint64, string, time.Time) (*domain.SignedUploadPartURL, error)
}

// NewUploadPartURLService 绑定签名目标查询和存储单片签名方法。
func NewUploadPartURLService(
	getTarget func(context.Context, domain.Actor, uint64) (*UploadPartSigningTarget, error),
	signPart func(context.Context, string, string, uint32, uint64, string, time.Time) (*domain.SignedUploadPartURL, error),
) *UploadPartURLService {
	if getTarget == nil || signPart == nil {
		panic("分片地址签发服务需要目标读取和单片签名方法")
	}
	return &UploadPartURLService{getTarget: getTarget, signPart: signPart}
}

// Create 按当前任务期限签发一批地址，只有整批完成并通过复查时才交付。
func (urlService *UploadPartURLService) Create(ctx context.Context, actor domain.Actor, taskID uint64, parts []UploadPartDeclaration) (*UploadPartURLBatch, error) {
	// 1. 按认证账号读取目标，验证整批声明并计算每片长度。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || taskID == 0 {
		return nil, fmt.Errorf("签发分片地址需要上下文、完整认证身份和非零任务 ID")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	loaded, err := urlService.getTarget(ctx, actor, taskID)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, err
	}
	if loaded == nil || loaded.TaskID != taskID || loaded.UserID != actor.UserID || loaded.StagedObjectID == 0 ||
		loaded.ObjectKey == "" || !domain.ValidMultipartUploadID(loaded.UploadID) || !validUploadDatabaseTime(loaded.ExpiresAt) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	target := *loaded
	target.ExpiresAt = target.ExpiresAt.UTC()
	inputs, err := PrepareUploadPartSigningInputs(target.FileSizeBytes, target.PartSizeBytes, target.PartCount, parts)
	if err != nil {
		return nil, err
	}

	// 2. 逐片签名并暂存结果，任一片失败都会中止整批。
	result := &UploadPartURLBatch{ExpiresAt: target.ExpiresAt, Parts: make([]UploadPartURL, 0, len(inputs))}
	earliestExpiry := target.ExpiresAt
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		signed, err := urlService.signPart(ctx, target.ObjectKey, target.UploadID, input.PartNumber, input.SizeBytes, input.ChecksumSHA256, target.ExpiresAt)
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		if err != nil {
			return nil, err
		}
		if signed == nil || !signed.IsValid() || signed.ExpiresAt.After(target.ExpiresAt) {
			return nil, domain.ErrUploadStorageUnavailable
		}
		expiresAt := signed.ExpiresAt.UTC()
		if expiresAt.Before(earliestExpiry) {
			earliestExpiry = expiresAt
		}
		result.Parts = append(result.Parts, UploadPartURL{
			PartNumber: input.PartNumber, SizeBytes: input.SizeBytes, ChecksumSHA256: input.ChecksumSHA256,
			URL: signed.URL, URLExpiresAt: expiresAt,
		})
	}

	// 3. 重读任务和对象，复查绑定、租约版本、分片计划和期限，再交付整批。
	current, err := urlService.getTarget(ctx, actor, taskID)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	checkedTarget := *current
	checkedTarget.ExpiresAt = checkedTarget.ExpiresAt.UTC()
	if checkedTarget != target || !time.Now().Before(earliestExpiry) {
		return nil, domain.ErrUploadTaskNotReady
	}
	return result, nil
}
