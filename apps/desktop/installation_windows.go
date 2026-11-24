package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// loadInstallationID 返回当前 Windows 用户的 CloudNest 安装标识。
func loadInstallationID() (string, error) {
	localAppData := os.Getenv("LOCALAPPDATA")
	if !filepath.IsAbs(localAppData) {
		return "", errors.New("无法确定 Windows 本地应用数据目录")
	}
	return loadOrCreateInstallationID(filepath.Join(localAppData, "CloudNest", "installation-id"))
}

func loadOrCreateInstallationID(path string) (string, error) {
	// 1. 读取并校验已有标识，只有文件不存在时才创建。
	id, err := readInstallationID(path)
	if !errors.Is(err, os.ErrNotExist) {
		return id, err
	}

	// 2. 生成小写 UUID v4，先完整写入同目录的临时文件。
	generatedID, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("生成安装标识失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", fmt.Errorf("创建安装标识目录失败: %w", err)
	}
	temporaryFile, err := os.CreateTemp(filepath.Dir(path), ".installation-id-*")
	if err != nil {
		return "", fmt.Errorf("创建安装标识临时文件失败: %w", err)
	}
	defer os.Remove(temporaryFile.Name())
	defer temporaryFile.Close()
	if _, err := temporaryFile.WriteString(generatedID.String()); err != nil {
		return "", fmt.Errorf("写入安装标识失败: %w", err)
	}
	if err := temporaryFile.Sync(); err != nil {
		return "", fmt.Errorf("同步安装标识失败: %w", err)
	}
	if err := temporaryFile.Close(); err != nil {
		return "", fmt.Errorf("关闭安装标识临时文件失败: %w", err)
	}

	// 3. 通过硬链接发布完整文件。并发启动时只会有一个进程发布成功。
	if err := os.Link(temporaryFile.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("保存安装标识失败: %w", err)
	}
	return readInstallationID(path)
}

func readInstallationID(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取安装标识失败: %w", err)
	}
	text := string(content)
	id, err := uuid.Parse(text)
	if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || id.String() != text {
		return "", errors.New("安装标识文件需要包含标准小写 UUID v4")
	}
	return text, nil
}
