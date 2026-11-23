package main

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

// RootDirectoryResult 将根目录首页或重新登录提示交给桌面窗口。
type RootDirectoryResult struct {
	Page          client.DirectoryPage `json:"page"`
	LoginRequired string               `json:"loginRequired"`
}

// ReadRootDirectory 按当前账号的真实根目录 ID 读取首页。
func (account *Account) ReadRootDirectory(ctx context.Context) (RootDirectoryResult, error) {
	page, err := readForAccount(ctx, account, func(address string, session client.NativeSession) (client.DirectoryPage, error) {
		// 1. 从本人资料取得根目录 ID，核对资料属于当前会话。
		profile, err := client.ReadCurrentProfile(ctx, address, session.AccessToken)
		if err != nil {
			return client.DirectoryPage{}, err
		}
		if profile.Username != session.Username {
			return client.DirectoryPage{}, errors.New("账号资料与当前会话不一致")
		}

		// 2. 使用同一会话读取目录，校验返回路径确实是根目录。
		page, err := client.ReadDirectoryPage(ctx, address, session.AccessToken, profile.RootEntryID)
		if err != nil {
			return client.DirectoryPage{}, err
		}
		if len(page.Breadcrumbs) != 1 || page.Breadcrumbs[0].Name != "" {
			return client.DirectoryPage{}, errors.New("服务返回的路径与根目录不一致")
		}
		return page, nil
	})

	// 3. 会话失效时让页面重回登录，否则交付目录首页或读取错误。
	if errors.Is(err, errLoginRequired) || errors.Is(err, errRefreshOutcomeUnknown) {
		return RootDirectoryResult{LoginRequired: err.Error()}, nil
	}
	return RootDirectoryResult{Page: page}, err
}
