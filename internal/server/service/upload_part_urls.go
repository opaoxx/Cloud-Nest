package service

import (
	"context"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadPartURL 保存客户端上传一片所需的签名和校验参数。
type UploadPartURL = domain.UploadPartURL

// UploadPartURLBatch 保存当前任务的期限和按请求顺序签发的链接。
type UploadPartURLBatch = domain.UploadPartURLBatch

// UploadPartURLService 组合目标读取、可选续期、逐片签名和交付前复查。
type UploadPartURLService struct {
	getTarget   func(context.Context, domain.Actor, uint64) (*UploadPartSigningTarget, error)
	signPart    func(context.Context, string, string, uint32, uint64, string, time.Time) (*domain.SignedUploadPartURL, error)
	renewExpiry func(context.Context, domain.Actor, uint64, time.Time,
		func(*domain.UploadTask, *domain.UserProfile, time.Time, time.Time) (time.Time, error)) (*domain.UploadTask, error)
}

// NewUploadPartURLService 绑定目标查询、单片签名和提交后返回任务的续期事务。
func NewUploadPartURLService(
	getTarget func(context.Context, domain.Actor, uint64) (*UploadPartSigningTarget, error),
	signPart func(context.Context, string, string, uint32, uint64, string, time.Time) (*domain.SignedUploadPartURL, error),
	renewExpiry func(context.Context, domain.Actor, uint64, time.Time,
		func(*domain.UploadTask, *domain.UserProfile, time.Time, time.Time) (time.Time, error)) (*domain.UploadTask, error),
) *UploadPartURLService {
	if getTarget == nil || signPart == nil || renewExpiry == nil {
		panic("分片地址签发服务需要目标读取、单片签名和续期事务")
	}
	return &UploadPartURLService{getTarget: getTarget, signPart: signPart, renewExpiry: renewExpiry}
}

// Create 按当前任务期限签发一批地址。expectedExpiry 非空时先尝试显式续期。
// 续期一旦提交，后续签名失败时仍保留已提交的期限。
func (urlService *UploadPartURLService) Create(
	ctx context.Context, actor domain.Actor, taskID uint64, parts []UploadPartDeclaration, expectedExpiry *time.Time,
) (*UploadPartURLBatch, error) {
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

	// 2. 整批校验通过后才允许显式续期，用提交后的任务期限继续。
	if expectedExpiry != nil {
		if !expectedExpiry.Equal(target.ExpiresAt) {
			return nil, domain.ErrUploadTaskNotReady
		}
		renewed, err := urlService.renewExpiry(ctx, actor, taskID, *expectedExpiry,
			func(task *domain.UploadTask, account *domain.UserProfile, expected, databaseTime time.Time) (time.Time, error) {
				expiry, err := ResolveUploadPartRenewalExpiry(task, account, expected, databaseTime)
				if err != nil {
					return time.Time{}, err
				}
				if !target.matchesTaskBinding(task) {
					return time.Time{}, domain.ErrUploadTaskNotReady
				}
				return expiry, nil
			})
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		if err != nil {
			return nil, err
		}
		if !target.matchesTaskBinding(renewed) || !validUploadDatabaseTime(renewed.ExpiresAt) ||
			renewed.ExpiresAt.Before(target.ExpiresAt) {
			return nil, domain.ErrInvalidUploadTaskData
		}
		target.ExpiresAt = renewed.ExpiresAt.UTC()
	}

	// 3. 在事务外逐片签名并暂存结果，任一片失败都会中止整批。
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

	// 4. 重读任务和对象，复查绑定、租约版本、分片计划和期限，再交付整批。
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

// matchesTaskBinding 确认续期时的任务仍对应本次校验过的存储绑定与分片计划。
func (target UploadPartSigningTarget) matchesTaskBinding(task *domain.UploadTask) bool {
	return task != nil && task.ID == target.TaskID && task.UserID == target.UserID &&
		task.UploadMode == "MULTIPART" && task.Status == domain.UploadStatusUploading &&
		task.StagedObjectID == target.StagedObjectID && task.LeaseVersion == target.LeaseVersion &&
		task.StorageUploadID != nil && *task.StorageUploadID == target.UploadID && task.FileSize == target.FileSizeBytes &&
		task.PartSize != nil && *task.PartSize == target.PartSizeBytes && task.PartCount != nil && *task.PartCount == target.PartCount
}
