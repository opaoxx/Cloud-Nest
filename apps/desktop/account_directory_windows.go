package main

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

// DirectoryReadResult 将目录首页或重新登录提示交给桌面窗口。
type DirectoryReadResult struct {
	Page          client.DirectoryPage `json:"page"`
	LoginRequired string               `json:"loginRequired"`
}

// ReadRootDirectory 按当前账号的真实根目录 ID 读取首页。
func (account *Account) ReadRootDirectory(ctx context.Context) (DirectoryReadResult, error) {
	return account.ReadDirectory(ctx, "")
}

// ReadDirectory 读取指定目录首页。空 ID 表示先从本人资料取得根目录。
func (account *Account) ReadDirectory(ctx context.Context, parentID string) (DirectoryReadResult, error) {
	page, err := readForAccount(ctx, account, func(address string, session client.NativeSession) (client.DirectoryPage, error) {
		// 1. 首次进入时取得账号根目录。后续导航使用窗口传入的条目 ID。
		targetID := parentID
		if targetID == "" {
			profile, err := client.ReadCurrentProfile(ctx, address, session.AccessToken)
			if err != nil {
				return client.DirectoryPage{}, err
			}
			if profile.Username != session.Username {
				return client.DirectoryPage{}, errors.New("账号资料与当前会话不一致")
			}
			targetID = profile.RootEntryID
		}

		// 2. 使用同一会话读取目录。首次进入时确认路径只含根目录。
		page, err := client.ReadDirectoryPage(ctx, address, session.AccessToken, targetID)
		if err != nil {
			return client.DirectoryPage{}, err
		}
		if parentID == "" && len(page.Breadcrumbs) != 1 {
			return client.DirectoryPage{}, errors.New("服务返回的路径与根目录不一致")
		}
		return page, nil
	})

	// 3. 会话失效时让页面重回登录，否则交付目录首页或读取错误。
	if errors.Is(err, errLoginRequired) || errors.Is(err, errRefreshOutcomeUnknown) {
		return DirectoryReadResult{LoginRequired: err.Error()}, nil
	}
	return DirectoryReadResult{Page: page}, err
}
