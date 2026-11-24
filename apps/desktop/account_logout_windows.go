package main

import (
	"context"
	"errors"
	"os"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

// Logout 清理当前账号的本地会话，并返回远端撤销的结果提示。
func (account *Account) Logout(ctx context.Context) (string, error) {
	account.mu.Lock()
	defer account.mu.Unlock()

	// 1. 在文件锁内核对归属，清除当前会话的本机凭证并废弃旧读取。
	session, address := account.session, account.address
	if session.AccessToken == "" {
		return "当前已退出账号。", nil
	}
	lock, err := account.credentials.lock()
	if err != nil {
		return "", err
	}
	defer lock.Close()
	record, err := account.credentials.read()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("无法核对本机凭证，请检查数据目录后重试退出")
	}
	anotherSession := err == nil && record != (savedSession{Address: address, Username: session.Username, RefreshToken: session.RefreshToken})
	if err == nil && !anotherSession {
		if err := os.Remove(account.credentials.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", errors.New("无法清除本机凭证，请检查数据目录后重试退出")
		}
	}
	account.forgetSession()

	// 2. 请求远端撤销。访问令牌明确过期时轮换一次，新凭证仅供该次退出使用。
	remoteError := client.LogoutNative(ctx, address, session.AccessToken)
	if errors.Is(remoteError, client.ErrAccessTokenExpired) && !anotherSession {
		rotated, refreshError := client.RefreshNative(ctx, address, session.RefreshToken)
		remoteError = refreshError
		if refreshError == nil {
			remoteError = client.LogoutNative(ctx, address, rotated.AccessToken)
		}
	}

	// 3. 返回本机退出结果及远端状态，便于用户判断是否已撤销。
	notice := "已退出账号，服务端会话已结束。"
	if remoteError != nil {
		notice = "已退出本机，未能确认服务端会话已撤销。"
	}
	if anotherSession {
		notice += "已保留其他窗口保存的会话记录。"
	}
	return notice, nil
}
