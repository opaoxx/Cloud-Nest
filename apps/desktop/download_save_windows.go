package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"
	"golang.org/x/sys/windows"
)

// DownloadSaver opens a native dialog and publishes a verified download.
type DownloadSaver struct {
	account *Account
	window  application.Window
}

var errSaveTargetExists = errors.New("目标已存在，请另存为其他名称")

// Save returns the destination path after publishing, or an empty path on cancel.
func (saver *DownloadSaver) Save(ctx context.Context, entryID, revision, objectID, suggestedName string) (string, error) {
	account := saver.account
	account.downloadMu.Lock()
	defer account.downloadMu.Unlock()

	// 1. 核对当前会话持有的暂存和用户所选文件，然后打开另存为窗口。
	account.mu.Lock()
	download := account.stagedDownload
	account.mu.Unlock()
	if download == nil || download.entryID != entryID || download.result.Revision != revision || download.result.ObjectID != objectID {
		return "", errors.New("所选文件的暂存已失效，请重新下载")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !validSaveFilename(suggestedName) {
		suggestedName = "CloudNest 下载"
	}
	path, err := application.Get().Dialog.SaveFileWithOptions(&application.SaveFileDialogOptions{
		Title:                "另存为（同名请选择其他名称）",
		Filename:             suggestedName,
		Window:               saver.window,
		CanCreateDirectories: true,
	}).PromptForSingleSelection()
	if err != nil {
		if err.Error() == "cancelled by user" {
			return "", nil
		}
		return "", errors.New("无法打开另存为窗口，请重试")
	}
	if path == "" {
		return "", nil
	}

	// 2. 在目标目录复核一份完整副本，失败时保留原下载以便重试。
	temporaryPath, err := copyForDownloadSave(ctx, download, path)
	if err != nil {
		return "", err
	}
	defer os.Remove(temporaryPath)

	// 3. 交付前再次核对会话，以禁止覆盖的方式提交最终名称。
	account.mu.Lock()
	defer account.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if account.stagedDownload != download {
		return "", errors.New("所选文件的暂存已失效，请重新下载")
	}
	if err := publishDownload(temporaryPath, path); err != nil {
		return "", err
	}
	account.discardStagedDownload()
	return path, nil
}

func validSaveFilename(name string) bool {
	return filepath.IsLocal(name) && filepath.Base(name) == name && strings.TrimRight(name, " .") == name &&
		!strings.ContainsAny(name, "<>:\"/\\|?*") && !strings.ContainsFunc(name, func(character rune) bool { return character < 32 })
}

func copyForDownloadSave(ctx context.Context, download *stagedDownload, destination string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(destination) || !validSaveFilename(filepath.Base(destination)) {
		return "", errors.New("文件名或保存位置无效，请重新选择")
	}
	if _, err := os.Lstat(destination); err == nil {
		return "", errSaveTargetExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("无法检查保存位置，请重新选择")
	}

	// 1. 使用目标目录中的独立临时文件，用户指定的文件名此时还未占用。
	temporaryFile, err := os.CreateTemp(filepath.Dir(destination), ".cloudnest-save-*.part")
	if err != nil {
		return "", errors.New("无法在目标目录创建临时文件，请检查可用空间和权限")
	}
	temporaryPath := temporaryFile.Name()
	keepFile := false
	defer func() {
		if !keepFile {
			temporaryFile.Close()
			os.Remove(temporaryPath)
		}
	}()
	expectedSize := int64(download.result.SizeBytes)
	source := io.NewSectionReader(download.file, 0, expectedSize)
	if copied, err := io.Copy(temporaryFile, source); err != nil || copied != expectedSize {
		return "", errors.New("无法完整复制下载文件，请检查可用空间后重试")
	}
	if err := temporaryFile.Sync(); err != nil {
		return "", errors.New("无法同步保存副本，请检查目标存储")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// 2. 从目标位置读回副本并核对完整内容，再交给改名步骤。
	info, err := temporaryFile.Stat()
	if err != nil || info.Size() != expectedSize {
		return "", errors.New("保存副本大小校验失败，请重试")
	}
	if _, err := temporaryFile.Seek(0, io.SeekStart); err != nil {
		return "", errors.New("无法读取保存副本，请重试")
	}
	digest, err := digestSourceSnapshot(temporaryFile, info)
	if err != nil || digest.SHA256 != download.result.SHA256 {
		return "", errors.New("保存副本 SHA-256 校验失败，请重试")
	}
	if err := temporaryFile.Close(); err != nil {
		return "", errors.New("无法关闭保存副本，请重试")
	}
	keepFile = true
	return temporaryPath, nil
}

func publishDownload(temporaryPath, destination string) error {
	source, err := windows.UTF16PtrFromString(temporaryPath)
	if err != nil {
		return errors.New("保存副本路径无效，请重新选择位置")
	}
	target, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return errors.New("保存目标路径无效，请重新选择位置")
	}
	// 不传替换或跨卷复制标志，提交时仍由 Windows 检查同名冲突。
	err = windows.MoveFileEx(source, target, windows.MOVEFILE_WRITE_THROUGH)
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrExist) {
		return errSaveTargetExists
	}
	if _, statErr := os.Lstat(destination); statErr == nil {
		return errSaveTargetExists
	}
	return errors.New("无法交付下载文件，请检查目标目录后重试")
}
