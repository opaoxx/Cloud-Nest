package storage

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const (
	// downloadURLLifetime 是首期短期 GET 签名的有效期。
	downloadURLLifetime = 15 * time.Minute
	// sigV4SigningTimeLayout 是 SigV4 查询参数 X-Amz-Date 的 UTC 格式。
	sigV4SigningTimeLayout = "20060102T150405Z"
)

// SignDownloadURL 签发固定到文件对象及交付头值的短期 GET 地址。
// 调用方提供已校验的下载目标，以及用途对应的媒体类型和文件名响应头值。
func (client *Client) SignDownloadURL(ctx context.Context, target *domain.EntryDownloadTarget, contentType, disposition string) (*domain.SignedDownloadURL, error) {
	// 1. 校验对象位置、版本和响应头值。
	if ctx == nil || client == nil || client.publicSigner == nil || client.bucket == "" {
		return nil, fmt.Errorf("签发下载地址需要存储签名客户端和上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	options, err := client.downloadObjectOptions(target)
	if err != nil {
		return nil, err
	}
	for _, value := range []string{contentType, disposition} {
		if strings.TrimSpace(value) == "" || strings.ContainsFunc(value, func(character rune) bool {
			return character < 32 || character > 126
		}) {
			return nil, fmt.Errorf("下载签名需要有效的响应头值")
		}
	}

	// 2. 用客户端访问地址签发绑定这些参数的 GET URL。
	parameters := url.Values{
		"response-content-type":        {contentType},
		"response-content-disposition": {disposition},
	}
	if options.VersionID != "" {
		parameters.Set("versionId", options.VersionID)
	}
	signed, err := client.publicSigner.PresignedGetObject(ctx, client.bucket, target.ObjectKey, downloadURLLifetime, parameters)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, storageObjectError(err, domain.ErrDownloadStorageUnavailable)
	}

	// 3. 根据签名时间计算 UTC 到期时间。
	signedAt, err := time.Parse(sigV4SigningTimeLayout, signed.Query().Get("X-Amz-Date"))
	if err != nil {
		return nil, domain.ErrDownloadStorageUnavailable
	}
	return &domain.SignedDownloadURL{URL: signed.String(), ExpiresAt: signedAt.Add(downloadURLLifetime)}, nil
}
