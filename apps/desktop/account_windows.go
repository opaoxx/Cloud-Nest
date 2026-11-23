package main

import (
	"context"
	"errors"
	"sync"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

// Account 为桌面页面提供登录入口，并保存本次进程的认证会话。
type Account struct {
	mu             sync.Mutex
	installationID string
	session        client.NativeSession
}

// Login 建立本次会话，只把已登录的用户名返回给页面。
func (account *Account) Login(ctx context.Context, address, username, password string) (string, error) {
	account.mu.Lock()
	defer account.mu.Unlock()

	// 1. 确认本次安装可用，并避免重复提交覆盖已登录的会话。
	if account.installationID == "" {
		return "", errors.New("安装标识不可用，请在托盘的“关于 CloudNest”中查看原因")
	}
	if account.session.AccessToken != "" {
		return "", errors.New("当前已登录")
	}

	// 2. 调用 Go 客户端验证账号，失败时让页面保持可重试状态。
	session, err := client.LoginWindows(ctx, address, account.installationID, username, password)
	if err != nil {
		return "", err
	}

	// 3. 将会话保存在 Go 内存，只返回展示信息。
	account.session = session
	return session.Username, nil
}
