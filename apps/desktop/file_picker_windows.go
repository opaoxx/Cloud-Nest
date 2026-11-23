package main

import (
	"context"
	"errors"
	"sync"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// LocalFilePicker 负责在主窗口打开 Windows 单文件选择对话框。
type LocalFilePicker struct {
	window application.Window
	mu     sync.Mutex
	picked selectedUploadSource
}

type selectedUploadSource struct {
	Path string
	File FileSelection
}

// FileSelection 将文件摘要和本次选择的创建键交给窗口。
type FileSelection struct {
	SourceFileDigest
	CreationKey string `json:"creation_key"`
}

// SelectFile 返回所选文件的摘要，取消时返回 nil。
func (picker *LocalFilePicker) SelectFile(ctx context.Context) (*FileSelection, error) {
	// 1. 同一时刻只打开一个对话框，将它挂在当前桌面主窗口上。
	if !picker.mu.TryLock() {
		return nil, errors.New("文件正在选择或计算摘要")
	}
	defer picker.mu.Unlock()
	picker.picked = selectedUploadSource{}
	path, err := application.Get().Dialog.OpenFile().
		AttachToWindow(picker.window).
		SetTitle("选择本机文件").
		CanChooseFiles(true).
		CanChooseDirectories(false).
		PromptForSingleSelection()

	// 2. Wails v3.0.0 的 Windows 对话框以内部错误文本表达用户取消。
	if err != nil {
		if err.Error() == "cancelled by user" {
			return nil, nil
		}
		return nil, errors.New("无法打开文件选择窗口，请重试")
	}
	if path == "" {
		return nil, nil
	}

	// 3. 为此次选择生成固定创建键，源路径留在 Go 层。
	digest, err := readSourceFileDigest(ctx, path)
	if err != nil {
		return nil, err
	}
	key, err := uuid.NewRandom()
	if err != nil {
		return nil, errors.New("无法生成上传创建键，请重新选择")
	}
	selection := FileSelection{SourceFileDigest: *digest, CreationKey: key.String()}
	picker.picked = selectedUploadSource{Path: path, File: selection}
	return &selection, nil
}

func (picker *LocalFilePicker) selectedSource(creationKey string) (selectedUploadSource, error) {
	picker.mu.Lock()
	defer picker.mu.Unlock()
	if creationKey == "" || picker.picked.File.CreationKey != creationKey {
		return selectedUploadSource{}, errors.New("文件选择已变化，请重新选择")
	}
	return picker.picked, nil
}
