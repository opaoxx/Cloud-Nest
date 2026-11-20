package service

import (
	"context"
	"fmt"
	"net/url"
	"strings"

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
}

// NewEntryDownloadService 绑定下载目标读取、元数据检查和签名方法。
func NewEntryDownloadService(
	getTarget func(context.Context, domain.Actor, uint64, uint64) (*domain.EntryDownloadTarget, error),
	checkObject func(context.Context, *domain.EntryDownloadTarget) error,
	signURL func(context.Context, *domain.EntryDownloadTarget, string, string) (*domain.SignedDownloadURL, error),
) *EntryDownloadService {
	// 1. 检查并保存三个阶段所需的依赖。
	if getTarget == nil || checkObject == nil || signURL == nil {
		panic("下载授权服务需要目标读取、对象检查和签名方法")
	}
	return &EntryDownloadService{getTarget: getTarget, checkObject: checkObject, signURL: signURL}
}

// Create 为已认证 Actor 的指定文件版本签发下载或预览授权。
// 任一阶段失败或请求取消时返回 nil 与原错误类别。
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

	// 4. 核对签名结果，交付下载授权信息。
	if link == nil || link.ExpiresAt.IsZero() {
		return nil, domain.ErrDownloadStorageUnavailable
	}
	address, err := url.Parse(link.URL)
	if err != nil || (address.Scheme != "http" && address.Scheme != "https") || address.Hostname() == "" ||
		address.User != nil || strings.ContainsAny(link.URL, "# \r\n\t") {
		return nil, domain.ErrDownloadStorageUnavailable
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
