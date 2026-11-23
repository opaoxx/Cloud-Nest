package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/pkg/client"
	"golang.org/x/sys/windows"
)

// DownloadGrantResult 是窗口可展示的授权元数据。
type DownloadGrantResult struct {
	ObjectID      string `json:"object_id"`
	Revision      string `json:"revision"`
	SizeBytes     uint64 `json:"size_bytes"`
	SHA256        string `json:"sha256"`
	LoginRequired string `json:"loginRequired"`
}

type stagedDownload struct {
	file    *os.File
	entryID string
	result  DownloadGrantResult
}

// AuthorizeDownload 按窗口所选修订为当前账号取得下载授权。
func (account *Account) AuthorizeDownload(ctx context.Context, entryID, revision string) (DownloadGrantResult, error) {
	// 1. 沿用共享会话。令牌过期时最多刷新一次，并在同一修订上重试。
	grant, err := account.readDownloadGrant(ctx, entryID, revision)
	if errors.Is(err, errLoginRequired) || errors.Is(err, errRefreshOutcomeUnknown) {
		return DownloadGrantResult{LoginRequired: err.Error()}, nil
	}
	if err != nil {
		return DownloadGrantResult{}, err
	}

	// 2. 窗口只需要显示对象快照，签名地址留在 Go 层。
	return downloadGrantView(grant), nil
}

func (account *Account) readDownloadGrant(ctx context.Context, entryID, revision string) (client.DownloadGrant, error) {
	return readForAccount(ctx, account, func(address string, session client.NativeSession) (client.DownloadGrant, error) {
		return client.CreateDownloadGrant(ctx, address, session.AccessToken, entryID, revision)
	})
}

func downloadGrantView(grant client.DownloadGrant) DownloadGrantResult {
	return DownloadGrantResult{
		ObjectID: grant.ObjectID, Revision: grant.Revision, SizeBytes: grant.SizeBytes, SHA256: grant.SHA256,
	}
}

// StageDownload 在本机保留一份已经校验的临时文件，供当前会话保存。
func (account *Account) StageDownload(ctx context.Context, entryID, revision, objectID string) (DownloadGrantResult, error) {
	account.downloadMu.Lock()
	defer account.downloadMu.Unlock()
	if err := ctx.Err(); err != nil {
		return DownloadGrantResult{}, err
	}

	// 1. 记录当前会话并清除上一份暂存。下载时为同一修订获取新签名。
	account.mu.Lock()
	epoch := account.sessionEpoch
	account.discardStagedDownload()
	account.mu.Unlock()
	grant, err := account.readDownloadGrant(ctx, entryID, revision)
	if errors.Is(err, errLoginRequired) || errors.Is(err, errRefreshOutcomeUnknown) {
		return DownloadGrantResult{LoginRequired: err.Error()}, nil
	}
	if err != nil {
		return DownloadGrantResult{}, err
	}
	if grant.ObjectID != objectID {
		return DownloadGrantResult{}, errors.New("下载对象已变化，请重新选择文件")
	}

	// 2. 正文进入应用数据目录。失败时关闭句柄，由 Windows 删除临时文件。
	file, err := openDownloadTemporaryFile(filepath.Dir(account.credentials.path))
	if err != nil {
		return DownloadGrantResult{}, err
	}
	keepFile := false
	defer func() {
		if !keepFile {
			file.Close()
		}
	}()
	if err := client.DownloadContent(ctx, grant, file); err != nil {
		return DownloadGrantResult{}, err
	}
	if err := file.Sync(); err != nil {
		return DownloadGrantResult{}, errors.New("无法同步临时文件，请检查本机存储")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return DownloadGrantResult{}, errors.New("无法定位已校验的临时文件")
	}

	// 3. 同一会话才接收校验结果，保留打开的文件供保存步骤从头读取。
	account.mu.Lock()
	defer account.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return DownloadGrantResult{}, err
	}
	if account.sessionEpoch != epoch {
		return DownloadGrantResult{}, errSessionChanged
	}
	result := downloadGrantView(grant)
	account.stagedDownload = &stagedDownload{file: file, entryID: entryID, result: result}
	keepFile = true
	return result, nil
}

// discardStagedDownload 必须在持有 account.mu 时调用。
func (account *Account) discardStagedDownload() {
	if account.stagedDownload != nil {
		account.stagedDownload.file.Close()
		account.stagedDownload = nil
	}
}

func openDownloadTemporaryFile(dataDirectory string) (*os.File, error) {
	if !filepath.IsAbs(dataDirectory) {
		return nil, errors.New("无法确定本机下载临时目录")
	}
	name, err := uuid.NewRandom()
	if err != nil {
		return nil, errors.New("无法生成下载临时文件名")
	}
	path := filepath.Join(dataDirectory, ".download-"+name.String()+".part")
	nativePath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, errors.New("无效的下载临时文件路径")
	}
	handle, err := windows.CreateFile(nativePath, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, nil, windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_TEMPORARY|windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
	if err != nil {
		return nil, errors.New("无法创建下载临时文件，请检查本机存储")
	}
	return os.NewFile(uintptr(handle), path), nil
}
