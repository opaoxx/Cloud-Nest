package main

import (
	"context"
	"errors"
	"os"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

// ProfileReadResult 将账号资料或需要重新登录的提示交给窗口。
type ProfileReadResult struct {
	Username      string `json:"username"`
	LoginRequired string `json:"loginRequired"`
}

var errLoginRequired = errors.New("当前会话不可用，请重新登录")
var errRefreshOutcomeUnknown = errors.New("无法确认刷新结果，请重新登录")
var errSessionChanged = errors.New("登录账号已更新，请重新读取资料")

// ReadProfile 使用当前会话读取本人资料，令牌过期时最多刷新并重试一次。
func (account *Account) ReadProfile(ctx context.Context) (ProfileReadResult, error) {
	username, err := readForAccount(ctx, account, func(address string, session client.NativeSession) (string, error) {
		username, err := client.ReadCurrentUsername(ctx, address, session.AccessToken)
		if err == nil && username != session.Username {
			return "", errors.New("账号资料与当前会话不一致")
		}
		return username, err
	})
	if errors.Is(err, errLoginRequired) || errors.Is(err, errRefreshOutcomeUnknown) {
		return ProfileReadResult{LoginRequired: err.Error()}, nil
	}
	return ProfileReadResult{Username: username}, err
}

func readForAccount[Result any](ctx context.Context, account *Account, read func(string, client.NativeSession) (Result, error)) (Result, error) {
	var empty Result
	// 1. 在锁内取得会话快照，释放锁后允许各读取请求独立执行。
	account.mu.Lock()
	address, session, epoch := account.address, account.session, account.sessionEpoch
	account.mu.Unlock()
	if session.AccessToken == "" {
		return empty, errLoginRequired
	}
	result, err := read(address, session)

	// 2. 只有 TOKEN_EXPIRED 触发刷新，后到的请求会复用已轮换的令牌。
	if errors.Is(err, client.ErrAccessTokenExpired) {
		session.AccessToken, err = account.refreshForRead(ctx, session.RefreshToken, epoch)
		if err == nil {
			result, err = read(address, session)
		}
	}
	if err != nil {
		return empty, err
	}

	// 3. 只交付当前会话的结果，避免之前账号的请求在重新登录后写入窗口。
	account.mu.Lock()
	defer account.mu.Unlock()
	if account.sessionEpoch != epoch {
		if account.session.AccessToken != "" {
			return empty, errSessionChanged
		}
		return empty, errLoginRequired
	}
	return result, nil
}

func (account *Account) refreshForRead(ctx context.Context, previousRefreshToken string, epoch uint64) (string, error) {
	account.mu.Lock()
	defer account.mu.Unlock()

	// 1. 等候期间会话可能已轮换或结束，先检查再决定是否发请求。
	if account.sessionEpoch != epoch {
		if account.session.AccessToken != "" {
			return "", errSessionChanged
		}
		return "", errLoginRequired
	}
	if account.session.RefreshToken != previousRefreshToken {
		return account.session.AccessToken, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// 2. 文件锁保护跨进程的读取和更新，核对磁盘上的凭证仍属于当前会话。
	lock, err := account.credentials.lock()
	if err != nil {
		return "", err
	}
	defer lock.Close()
	record, err := account.credentials.read()
	if err != nil || record != (savedSession{Address: account.address, Username: account.session.Username, RefreshToken: account.session.RefreshToken}) {
		account.forgetSession()
		return "", errLoginRequired
	}

	// 3. 刷新为共享操作，保持自身的 HTTP 超时，避免某个读取取消时中断新凭证落盘。
	session, err := account.rotateCredential(context.WithoutCancel(ctx), record)
	if err != nil {
		account.forgetSession()
		if removeErr := os.Remove(account.credentials.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return "", errors.Join(errRefreshOutcomeUnknown, errors.New("无法清除本机的凭证文件"))
		}
		if errors.Is(err, client.ErrRefreshRejected) {
			return "", errLoginRequired
		}
		return "", errRefreshOutcomeUnknown
	}
	account.session = session
	return session.AccessToken, nil
}

// forgetSession 清空内存，并让持有旧会话编号的读取失效。调用方需要持有账户锁。
func (account *Account) forgetSession() {
	account.discardStagedDownload()
	account.session = client.NativeSession{}
	account.address = ""
	account.sessionEpoch++
}
