package main

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

// DownloadGrantResult 是窗口可展示的授权元数据。
type DownloadGrantResult struct {
	ObjectID      string `json:"object_id"`
	Revision      string `json:"revision"`
	SizeBytes     uint64 `json:"size_bytes"`
	SHA256        string `json:"sha256"`
	LoginRequired string `json:"loginRequired"`
}

// AuthorizeDownload 按窗口所选修订为当前账号取得下载授权。
func (account *Account) AuthorizeDownload(ctx context.Context, entryID, revision string) (DownloadGrantResult, error) {
	// 1. 沿用共享会话。令牌过期时最多刷新一次，并在同一修订上重试。
	grant, err := readForAccount(ctx, account, func(address string, session client.NativeSession) (client.DownloadGrant, error) {
		return client.CreateDownloadGrant(ctx, address, session.AccessToken, entryID, revision)
	})
	if errors.Is(err, errLoginRequired) || errors.Is(err, errRefreshOutcomeUnknown) {
		return DownloadGrantResult{LoginRequired: err.Error()}, nil
	}
	if err != nil {
		return DownloadGrantResult{}, err
	}

	// 2. 窗口只需要显示对象快照，签名地址留在 Go 层。
	return DownloadGrantResult{
		ObjectID: grant.ObjectID, Revision: grant.Revision, SizeBytes: grant.SizeBytes, SHA256: grant.SHA256,
	}, nil
}
