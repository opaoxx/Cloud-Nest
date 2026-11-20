package service

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// EntryDownloadService 为已认证用户组合下载目标检查与短期签名。
type EntryDownloadService struct {
	// getTarget 从同一快照取得当前用户有权访问的目标版本。
	getTarget func(context.Context, domain.Actor, uint64, uint64) (*domain.EntryDownloadTarget, error)
	// checkObject 核对该目标在 MinIO 中的版本与字节数。
	checkObject func(context.Context, *domain.EntryDownloadTarget) error
	// signURL 将同一个目标和交付头值绑定到短期链接。
	signURL func(context.Context, *domain.EntryDownloadTarget, string, string) (*domain.SignedDownloadURL, error)
	// withAccessTransaction 同步执行访问复查和写入，提交后返回 nil。
	withAccessTransaction func(context.Context, domain.Actor, func(context.Context, DownloadAccessTransaction) error) error
}

// NewEntryDownloadService 绑定下载目标读取、对象检查、签名及访问事务。
func NewEntryDownloadService(
	getTarget func(context.Context, domain.Actor, uint64, uint64) (*domain.EntryDownloadTarget, error),
	checkObject func(context.Context, *domain.EntryDownloadTarget) error,
	signURL func(context.Context, *domain.EntryDownloadTarget, string, string) (*domain.SignedDownloadURL, error),
	withAccessTransaction func(context.Context, domain.Actor, func(context.Context, DownloadAccessTransaction) error) error,
) *EntryDownloadService {
	// 1. 检查并保存授权阶段所需的依赖。
	if getTarget == nil || checkObject == nil || signURL == nil || withAccessTransaction == nil {
		panic("下载授权服务需要目标读取、对象检查、签名和访问事务")
	}
	return &EntryDownloadService{getTarget: getTarget, checkObject: checkObject, signURL: signURL, withAccessTransaction: withAccessTransaction}
}

// Create 为已认证 Actor 的指定文件版本签发下载或预览授权。
// 访问事务提交后才交付链接，失败或取消时返回 nil 与原错误类别。
func (downloadService *EntryDownloadService) Create(ctx context.Context, actor domain.Actor, entryID, expectedRevision uint64, purpose string) (*domain.EntryDownloadAuthorization, error) {
	// 1. 校验请求，读取当前用户的下载目标。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 || entryID == 0 || expectedRevision == 0 {
		return nil, fmt.Errorf("创建下载授权需要上下文、完整认证身份、非零目录项 ID 和预期版本")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidDownloadPurpose(purpose) {
		return nil, domain.ErrInvalidDownloadPurpose
	}
	target, err := downloadService.getTarget(ctx, actor, entryID, expectedRevision)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, err
	}
	if target == nil || target.EntryID != entryID || target.UserID != actor.UserID || target.Revision != expectedRevision || target.ObjectID == 0 {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 按用途确定媒体类型和文件名。
	contentType, err := ResolveDownloadContentType(purpose, target.MIMEType, target.SizeBytes)
	if err != nil {
		return nil, err
	}
	disposition, err := BuildDownloadDisposition(purpose, target.Name)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 3. 查验 MinIO 对象，通过后签发短期链接。
	objectError := downloadService.checkObject(ctx, target)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if objectError != nil {
		return nil, objectError
	}
	link, signError := downloadService.signURL(ctx, target, contentType, disposition)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if signError != nil {
		return nil, signError
	}

	// 4. 核对已经生成的签名结果。
	if link == nil || !link.IsValid() {
		return nil, domain.ErrDownloadStorageUnavailable
	}

	// 5. 复查并提交最近访问记录，成功后再交付链接。
	accessError := downloadService.recordDownloadAccess(ctx, actor, target)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if accessError != nil {
		return nil, accessError
	}
	return &domain.EntryDownloadAuthorization{
		Link:           domain.SignedDownloadURL{URL: link.URL, ExpiresAt: link.ExpiresAt.UTC()},
		ObjectID:       target.ObjectID,
		Revision:       target.Revision,
		SizeBytes:      target.SizeBytes,
		VerifiedSHA256: target.VerifiedSHA256,
		MIMEType:       contentType,
		Disposition:    disposition,
	}, nil
}
