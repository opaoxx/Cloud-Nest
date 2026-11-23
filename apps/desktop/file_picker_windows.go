package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// LocalFilePicker 负责在主窗口打开 Windows 单文件选择对话框。
type LocalFilePicker struct {
	window application.Window
	mu     sync.Mutex
}

// SelectFile 返回所选文件的名称，取消时返回空字符串。
func (picker *LocalFilePicker) SelectFile() (string, error) {
	// 1. 同一时刻只打开一个对话框，将它挂在当前桌面主窗口上。
	if !picker.mu.TryLock() {
		return "", errors.New("文件选择窗口已打开")
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
			return "", nil
		}
		return "", errors.New("无法打开文件选择窗口，请重试")
	}
	if path == "" {
		return "", nil
	}

	// 3. 仅检查所选绝对路径的元数据，向页面返回文件名称。
	if !filepath.IsAbs(path) {
		return "", errors.New("无法识别所选文件的位置，请重新选择")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("无法访问所选文件，请重新选择一个普通文件")
	}
	return info.Name(), nil
}
