package main

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/pkg/client"
)

// DirectoryReadResult 将目录页或重新登录提示交给桌面窗口。
type DirectoryReadResult struct {
	Page          client.DirectoryPage `json:"page"`
	LoginRequired string               `json:"loginRequired"`
}

// FileTrashResult 将已确认的回收或重新登录提示交给桌面窗口。
type FileTrashResult struct {
	LoginRequired string `json:"loginRequired"`
}

// FileEditResult 将确认后的文件详情或重新登录提示交给桌面窗口。
type FileEditResult struct {
	Entry         client.DirectoryEntry `json:"entry"`
	LoginRequired string                `json:"loginRequired"`
}

// RenameFile 使用窗口保存的原修订和请求键提交新名称。
func (account *Account) RenameFile(ctx context.Context, entryID, revision, parentID, name, key string) (FileEditResult, error) {
	return account.editFile(ctx, func(address string, session client.NativeSession) (client.DirectoryEntry, error) {
		return client.RenameFile(ctx, address, session.AccessToken, entryID, revision, parentID, name, key)
	})
}

// MoveFile 使用窗口保存的原修订和请求键提交目标父目录。
func (account *Account) MoveFile(ctx context.Context, entryID, revision, parentID, targetParentID, key string) (FileEditResult, error) {
	return account.editFile(ctx, func(address string, session client.NativeSession) (client.DirectoryEntry, error) {
		return client.MoveFile(ctx, address, session.AccessToken, entryID, revision, parentID, targetParentID, key)
	})
}

func (account *Account) editFile(ctx context.Context, mutate func(string, client.NativeSession) (client.DirectoryEntry, error)) (FileEditResult, error) {
	// 续期重试沿用原请求键。会话已失效时由窗口恢复登录页。
	entry, err := readForAccount(ctx, account, mutate)
	if errors.Is(err, errLoginRequired) || errors.Is(err, errRefreshOutcomeUnknown) || errors.Is(err, client.ErrFileSessionRevoked) {
		return FileEditResult{LoginRequired: err.Error()}, nil
	}
	return FileEditResult{Entry: entry}, err
}

// TrashFile 将选中的文件修订移入回收站。
func (account *Account) TrashFile(ctx context.Context, entryID, revision string) (FileTrashResult, error) {
	// 同一次操作的会话刷新重用相同的幂等键。
	key, err := uuid.NewRandom()
	if err != nil {
		return FileTrashResult{}, errors.New("无法准备文件回收请求")
	}
	_, err = readForAccount(ctx, account, func(address string, session client.NativeSession) (struct{}, error) {
		return struct{}{}, client.TrashFile(ctx, address, session.AccessToken, entryID, revision, key.String())
	})
	if errors.Is(err, errLoginRequired) || errors.Is(err, errRefreshOutcomeUnknown) || errors.Is(err, client.ErrFileSessionRevoked) {
		return FileTrashResult{LoginRequired: err.Error()}, nil
	}
	return FileTrashResult{}, err
}

// ReadRootDirectory 按当前账号的真实根目录 ID 读取首页。
func (account *Account) ReadRootDirectory(ctx context.Context) (DirectoryReadResult, error) {
	return account.ReadDirectory(ctx, "")
}

// ReadDirectory 读取指定目录首页。空 ID 表示先从本人资料取得根目录。
func (account *Account) ReadDirectory(ctx context.Context, parentID string) (DirectoryReadResult, error) {
	return account.readDirectory(ctx, parentID, "")
}

// ReadNextDirectoryPage 使用当前目录 ID 和上次返回的游标读取下一页。
func (account *Account) ReadNextDirectoryPage(ctx context.Context, parentID, cursor string) (DirectoryReadResult, error) {
	if parentID == "" || cursor == "" {
		return DirectoryReadResult{}, errors.New("续页需要当前目录和游标，请刷新目录")
	}
	return account.readDirectory(ctx, parentID, cursor)
}

func (account *Account) readDirectory(ctx context.Context, parentID, cursor string) (DirectoryReadResult, error) {
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

		// 2. 续页沿用目录 ID 和游标。首次进入时确认路径只含根目录。
		if cursor != "" {
			return client.ReadNextDirectoryPage(ctx, address, session.AccessToken, targetID, cursor)
		}
		page, err := client.ReadDirectoryPage(ctx, address, session.AccessToken, targetID)
		if err != nil {
			return client.DirectoryPage{}, err
		}
		if parentID == "" && len(page.Breadcrumbs) != 1 {
			return client.DirectoryPage{}, errors.New("服务返回的路径与根目录不一致")
		}
		return page, nil
	})

	// 3. 会话失效时让页面重回登录，否则交付目录页或读取错误。
	if errors.Is(err, errLoginRequired) || errors.Is(err, errRefreshOutcomeUnknown) {
		return DirectoryReadResult{LoginRequired: err.Error()}, nil
	}
	return DirectoryReadResult{Page: page}, err
}
