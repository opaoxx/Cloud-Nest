package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/opaoxx/cloud-nest/pkg/client"
	"golang.org/x/sys/windows"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type downloadFailure struct {
	code    string
	message string
}

func (failure *downloadFailure) Error() string { return failure.message }

var (
	errStageRangeInvalid = &downloadFailure{"RANGE_INVALID", client.ErrDownloadRangeMismatch.Error()}
	errStageDiskFull     = &downloadFailure{"STAGING_DISK_FULL", "本机下载暂存空间不足，请释放空间后继续下载"}
	errSaveDiskFull      = &downloadFailure{"SAVE_DISK_FULL", "保存位置空间不足，请释放空间或改选位置后重试"}
	errSaveTargetExists  = &downloadFailure{"SAVE_TARGET_EXISTS", "目标已存在，请另存为其他名称"}
	errDownloadExited    = &downloadFailure{"APPLICATION_EXITED", "上次退出时已保留下载进度，请继续下载"}
	errDownloadWoke      = &downloadFailure{"SYSTEM_WOKE", errTransferWake.Error()}
)

func downloadFailureMessage(code string) string {
	for _, failure := range []*downloadFailure{errStageRangeInvalid, errStageDiskFull, errSaveDiskFull, errSaveTargetExists,
		errDownloadExited, errDownloadWoke} {
		if failure.code == code {
			return failure.message
		}
	}
	return ""
}

func downloadFailureAfterInterruption(ctx context.Context, err error) error {
	var failure *downloadFailure
	if errors.As(err, &failure) {
		return err
	}
	switch transferInterruption(ctx) {
	case errTransferStopping:
		return errDownloadExited
	case errTransferWake:
		return errDownloadWoke
	default:
		return err
	}
}

func downloadDiskFull(err error) bool {
	var sqliteError *sqlite.Error
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL) ||
		errors.Is(err, windows.ERROR_DISK_QUOTA_EXCEEDED) ||
		(errors.As(err, &sqliteError) && sqliteError.Code() == sqlite3.SQLITE_FULL)
}

func downloadStorageError(err error, diskFull *downloadFailure, message string) error {
	if downloadDiskFull(err) {
		return diskFull
	}
	return errors.New(message)
}

func rememberDownloadFailure(ctx context.Context, db *sql.DB, fileID string, err error) error {
	// 1. 只保存可判定的失败原因，活跃的会话继续收到原错误供即时显示。
	var failure *downloadFailure
	if !errors.As(err, &failure) {
		return err
	}
	_, writeErr := db.ExecContext(ctx, `INSERT INTO download_failures (file_id, failure_code) VALUES (?, ?)
		ON CONFLICT(file_id) DO UPDATE SET failure_code = excluded.failure_code`, fileID, failure.code)
	if writeErr != nil {
		return fmt.Errorf("%w。本机暂时无法记录失败原因", err)
	}
	return err
}
