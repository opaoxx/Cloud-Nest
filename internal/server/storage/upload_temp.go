package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/opaoxx/cloud-nest/internal/server/config"
)

// PrepareUploadTempDirectory 创建上传临时目录并验证基本文件读写能力。
// uploadConfig 来自配置加载，成功后调用方使用其 Directory 装配接收服务。
// 取消在文件操作之间检查，探测文件在返回前关闭并删除。
func PrepareUploadTempDirectory(ctx context.Context, uploadConfig config.UploadTempConfig) (resultError error) {
	// 1. 检查上下文和绝对目录配置。
	if ctx == nil || !filepath.IsAbs(uploadConfig.Directory) {
		return fmt.Errorf("准备上传临时目录需要上下文和绝对目录配置")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// 2. 创建所需目录，并确认目标是目录。
	if err := os.MkdirAll(uploadConfig.Directory, 0700); err != nil {
		return fmt.Errorf("创建上传临时目录失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(uploadConfig.Directory)
	if err != nil {
		return fmt.Errorf("检查上传临时目录失败: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("上传临时目录路径必须指向目录")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// 3. 创建随机探测文件，登记关闭和删除。
	probe, err := os.CreateTemp(uploadConfig.Directory, ".cloudnest-upload-probe-*")
	if err != nil {
		return fmt.Errorf("创建上传目录探测文件失败: %w", err)
	}
	defer func() {
		if err := probe.Close(); err != nil {
			resultError = errors.Join(resultError, fmt.Errorf("关闭上传目录探测文件失败: %w", err))
		}
		if err := os.Remove(probe.Name()); err != nil {
			resultError = errors.Join(resultError, fmt.Errorf("删除上传目录探测文件失败: %w", err))
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}

	// 4. 写入、同步并读回探测内容。
	const probeContent = "cloudnest-upload-probe\n"
	if _, err := probe.WriteString(probeContent); err != nil {
		return fmt.Errorf("写入上传目录探测文件失败: %w", err)
	}
	if err := probe.Sync(); err != nil {
		return fmt.Errorf("同步上传目录探测文件失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := probe.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("定位上传目录探测文件失败: %w", err)
	}
	content, err := io.ReadAll(io.LimitReader(probe, int64(len(probeContent)+1)))
	if err != nil {
		return fmt.Errorf("读取上传目录探测文件失败: %w", err)
	}

	// 5. 核对内容与取消状态，返回初始化结果。
	if err := ctx.Err(); err != nil {
		return err
	}
	if string(content) != probeContent {
		return fmt.Errorf("上传目录探测文件内容校验失败")
	}
	return nil
}
