package main

import (
	"errors"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// LocalFilePicker 负责在主窗口打开 Windows 单文件选择对话框。
type LocalFilePicker struct {
	window application.Window
	mu     sync.Mutex
}

// SelectFile 返回所选文件的摘要，取消时返回 nil。
func (picker *LocalFilePicker) SelectFile() (*SourceFileDigest, error) {
	// 1. 同一时刻只打开一个对话框，将它挂在当前桌面主窗口上。
	if !picker.mu.TryLock() {
		return nil, errors.New("文件正在选择或计算摘要")
	}
	defer picker.mu.Unlock()
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

	// 3. 在 Go 中读取所选文件，只将文件名、实际大小和摘要交给页面。
	return readSourceFileDigest(path)
}
