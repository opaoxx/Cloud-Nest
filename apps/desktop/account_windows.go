package main

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

// Account 为桌面页面提供登录入口，并保存本次进程的认证会话。
type Account struct {
	mu             sync.Mutex
	installationID string
	credentials    sessionFile
	session        client.NativeSession
	address        string
}

// RestoredAccount 是启动时可交给桌面页面的会话恢复信息。
type RestoredAccount struct {
	Address  string `json:"address"`
	Username string `json:"username"`
	Notice   string `json:"notice"`
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
	lock, err := account.credentials.lock()
	if err != nil {
		return "", err
	}
	defer lock.Close()

	// 2. 调用 Go 客户端验证账号，失败时让页面保持可重试状态。
	session, err := client.LoginWindows(ctx, address, account.installationID, username, password)
	if err != nil {
		return "", err
	}

	// 3. 保存受保护的刷新凭证，随后将本次会话留在 Go 内存。
	if err := account.credentials.write(savedSession{Address: address, Username: session.Username, RefreshToken: session.RefreshToken}); err != nil {
		return "", errors.New("无法保存刷新凭证，请检查本机数据目录后重新登录")
	}
	account.session = session
	account.address = address
	return session.Username, nil
}

// Restore 读取 Windows 保护的凭证，尝试恢复本次进程的会话。
func (account *Account) Restore(ctx context.Context) (RestoredAccount, error) {
	account.mu.Lock()
	defer account.mu.Unlock()

	// 1. 复用本进程已恢复的会话，或独占读取磁盘上的记录。
	if account.session.AccessToken != "" {
		return RestoredAccount{Address: account.address, Username: account.session.Username}, nil
	}
	if account.installationID == "" {
		return RestoredAccount{}, errors.New("安装标识不可用，请在托盘的“关于 CloudNest”中查看原因")
	}
	lock, err := account.credentials.lock()
	if err != nil {
		return RestoredAccount{}, err
	}
	defer lock.Close()
	record, err := account.credentials.read()
	if errors.Is(err, os.ErrNotExist) {
		return RestoredAccount{}, nil
	}
	if err != nil {
		return RestoredAccount{}, errors.New("无法读取已保存的会话，请重新登录")
	}

	// 2. 使用记录内的原服务地址刷新。只在凭证明确被拒绝时删除记录。
	view := RestoredAccount{Address: record.Address}
	session, err := client.RefreshNative(ctx, record.Address, record.RefreshToken)
	if err != nil {
		view.Notice = err.Error()
		if errors.Is(err, client.ErrRefreshRejected) {
			if removeError := os.Remove(account.credentials.path); removeError != nil && !errors.Is(removeError, os.ErrNotExist) {
				view.Notice = "已保存的会话已失效，且无法清除本机记录。请重新登录"
			}
		}
		return view, nil
	}

	// 3. 先保存替换后的凭证，再公布会话恢复成功。
	record.RefreshToken = session.RefreshToken
	if err := account.credentials.write(record); err != nil {
		view.Notice = "无法保存轮换后的刷新凭证，请重新登录"
		return view, nil
	}
	session.Username = record.Username
	account.session = session
	account.address = record.Address
	view.Username = record.Username
	return view, nil
}
