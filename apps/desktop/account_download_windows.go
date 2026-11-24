package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

// DownloadGrantResult 是窗口可展示的授权元数据。
type DownloadGrantResult struct {
	ObjectID       string `json:"object_id"`
	Revision       string `json:"revision"`
	SizeBytes      uint64 `json:"size_bytes"`
	ConfirmedBytes uint64 `json:"confirmed_bytes"`
	SHA256         string `json:"sha256"`
	FailureMessage string `json:"failure_message,omitempty"`
	LoginRequired  string `json:"loginRequired"`
}

type stagedDownload struct {
	file         *os.File
	entryID      string
	checkpointID string
	result       DownloadGrantResult
}

// AuthorizeDownload 按窗口所选修订为当前账号取得下载授权。
func (account *Account) AuthorizeDownload(ctx context.Context, entryID, revision string) (DownloadGrantResult, error) {
	// 1. 沿用共享会话。令牌过期时最多刷新一次，并在同一修订上重试。
	authorization, err := account.readDownloadAuthorization(ctx, entryID, revision)
	if errors.Is(err, errLoginRequired) || errors.Is(err, errRefreshOutcomeUnknown) {
		return DownloadGrantResult{LoginRequired: err.Error()}, nil
	}
	if err != nil {
		return DownloadGrantResult{}, err
	}

	// 2. 仅显示与当前账号和对象一致的已保存断点，签名地址留在 Go 层。
	db, err := openDownloadCheckpoints(ctx, filepath.Dir(account.credentials.path))
	if err != nil {
		return DownloadGrantResult{}, err
	}
	defer db.Close()
	checkpoint, err := readDownloadCheckpoint(ctx, db, authorization)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return DownloadGrantResult{}, err
	}
	result := downloadGrantView(authorization.grant)
	result.ConfirmedBytes = uint64(checkpoint.confirmed)
	result.FailureMessage = downloadFailureMessage(checkpoint.failure)
	return result, nil
}

func (account *Account) readDownloadAuthorization(ctx context.Context, entryID, revision string) (downloadAuthorization, error) {
	return readForAccount(ctx, account, func(address string, session client.NativeSession) (downloadAuthorization, error) {
		owner, err := resolveUploadOwner(ctx, address, session)
		if err != nil {
			return downloadAuthorization{}, err
		}
		grant, err := client.CreateDownloadGrant(ctx, address, session.AccessToken, entryID, revision)
		return downloadAuthorization{owner: owner, entryID: entryID, grant: grant}, err
	})
}

func downloadGrantView(grant client.DownloadGrant) DownloadGrantResult {
	return DownloadGrantResult{
		ObjectID: grant.ObjectID, Revision: grant.Revision, SizeBytes: grant.SizeBytes, SHA256: grant.SHA256,
	}
}

// StageDownload 从本机断点恢复原文件，在校验完成后交给当前会话保存。
func (account *Account) StageDownload(ctx context.Context, entryID, revision, objectID string) (DownloadGrantResult, error) {
	return runDesktopTransfer(ctx, account.transfers, func(ctx context.Context) (DownloadGrantResult, error) {
		return account.stageDownload(ctx, entryID, revision, objectID)
	})
}

func (account *Account) stageDownload(ctx context.Context, entryID, revision, objectID string) (DownloadGrantResult, error) {
	account.downloadMu.Lock()
	defer account.downloadMu.Unlock()
	if err := ctx.Err(); err != nil {
		return DownloadGrantResult{}, err
	}

	// 1. 记录会话并关闭上一份暂存的句柄。继续同一修订前申请新的授权。
	account.mu.Lock()
	epoch := account.sessionEpoch
	account.discardStagedDownload()
	account.mu.Unlock()
	authorization, err := account.readDownloadAuthorization(ctx, entryID, revision)
	if errors.Is(err, errLoginRequired) || errors.Is(err, errRefreshOutcomeUnknown) {
		return DownloadGrantResult{LoginRequired: err.Error()}, nil
	}
	if err != nil {
		return DownloadGrantResult{}, err
	}
	grant := authorization.grant
	if grant.ObjectID != objectID {
		return DownloadGrantResult{}, errors.New("下载对象已变化，请重新选择文件")
	}
	account.mu.Lock()
	sameSession := account.sessionEpoch == epoch
	account.mu.Unlock()
	if !sameSession {
		return DownloadGrantResult{}, errSessionChanged
	}

	// 2. 在应用目录读取同一账号、条目、修订和对象的断点。失败时保留已同步的字节。
	directory := filepath.Dir(account.credentials.path)
	lock, err := lockDownloadCheckpoints(directory)
	if err != nil {
		return DownloadGrantResult{}, err
	}
	defer lock.Close()
	db, err := openDownloadCheckpoints(ctx, directory)
	if err != nil {
		return DownloadGrantResult{}, err
	}
	defer db.Close()
	checkpoint, err := readDownloadCheckpoint(ctx, db, authorization)
	if errors.Is(err, sql.ErrNoRows) {
		checkpoint, err = createDownloadCheckpoint(ctx, db, authorization)
	}
	if err != nil {
		return DownloadGrantResult{}, err
	}
	file, err := openDownloadCheckpointFile(directory, checkpoint.fileID)
	if err != nil {
		return DownloadGrantResult{}, rememberDownloadFailure(transferCheckpointContext(ctx), db, checkpoint.fileID, err)
	}
	keepFile := false
	defer func() {
		if !keepFile {
			file.Close()
		}
	}()
	writer := downloadCheckpointWriter{ctx: ctx, db: db, file: file, checkpoint: checkpoint}
	if err := stageDownloadCheckpoint(&writer, grant); err != nil {
		if errors.Is(err, errStageDiskFull) {
			// 仅保留数据库已经确认的前缀，尽量释放本轮未确认字节占用的空间。
			_ = file.Truncate(writer.checkpoint.confirmed)
		}
		return DownloadGrantResult{}, rememberDownloadFailure(transferCheckpointContext(ctx), db, checkpoint.fileID,
			downloadFailureAfterInterruption(ctx, err))
	}
	if checkpoint.failure == errStageRangeInvalid.code || checkpoint.failure == errStageDiskFull.code ||
		checkpoint.failure == errDownloadExited.code || checkpoint.failure == errDownloadWoke.code {
		if _, err := db.ExecContext(transferCheckpointContext(ctx), `DELETE FROM download_failures WHERE file_id = ?`, checkpoint.fileID); err != nil {
			return DownloadGrantResult{}, downloadStorageError(err, errStageDiskFull, "无法更新本机下载状态，请重试")
		}
	}

	// 3. 完整校验后才交给同一会话。另存为仍会核对目标位置中的副本。
	account.mu.Lock()
	defer account.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return DownloadGrantResult{}, err
	}
	if account.sessionEpoch != epoch {
		return DownloadGrantResult{}, errSessionChanged
	}
	result := downloadGrantView(grant)
	result.ConfirmedBytes = result.SizeBytes
	account.stagedDownload = &stagedDownload{file: file, entryID: entryID, checkpointID: checkpoint.fileID, result: result}
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
