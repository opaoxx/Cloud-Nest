package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// WithUploadContentFile 接收并校验单次上传，向 use 提供位于开头的完整临时文件。
// directory 由调用方提供应用专属目录，source 的生命周期由调用方管理。
// use 在回调期间读取文件，方法返回或发生 panic 时关闭并删除临时文件。
// 清理失败会合并到返回错误，调用方仍可通过 errors.Is 识别各项原因。
func WithUploadContentFile(ctx context.Context, directory string, source io.Reader, declaredSize uint64, expected *[sha256.Size]byte, use func(io.ReadSeeker, domain.UploadContentDigest) error) (resultError error) {
	// 1. 检查输入、声明上限和取消状态。
	if ctx == nil || directory == "" || source == nil || use == nil {
		return fmt.Errorf("接收上传临时文件需要上下文、临时目录、来源和文件使用方法")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if declaredSize > domain.MaxSingleUploadSizeBytes {
		return domain.ErrUploadFileTooLarge
	}
	expected = copyOptionalValue(expected)

	// 2. 创建随机临时文件，安排关闭和删除。
	file, err := os.CreateTemp(directory, "upload-*.part")
	if err != nil {
		return fmt.Errorf("创建上传临时文件失败: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			resultError = errors.Join(resultError, fmt.Errorf("关闭上传临时文件失败: %w", err))
		}
		if err := os.Remove(file.Name()); err != nil {
			resultError = errors.Join(resultError, fmt.Errorf("删除上传临时文件失败: %w", err))
		}
	}()

	// 3. 受限复制文件并校验预期摘要。
	digest, err := CopyUploadContent(ctx, file, source, declaredSize)
	if err != nil {
		return err
	}
	if err := ValidateUploadContentChecksum(digest.SHA256, expected); err != nil {
		return err
	}

	// 4. 同步文件并定位到开头。
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("同步上传临时文件失败: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("定位上传临时文件失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// 5. 调用文件使用方，复查取消并交付结果。
	useError := use(file, *digest)
	if err := ctx.Err(); err != nil {
		return errors.Join(useError, err)
	}
	return useError
}
