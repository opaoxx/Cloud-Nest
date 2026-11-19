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
func WithUploadContentFile(ctx context.Context, directory string, source io.Reader, declaredSize uint64, expected *[sha256.Size]byte, use func(io.ReadSeeker, domain.UploadContentDigest) error) error {
	return withUploadContentFile(ctx, directory, source, declaredSize, expected, nil, use, nil)
}

// withUploadContentFile 在清理结束时通知资源管理方，包含返回错误和 panic 路径。
func withUploadContentFile(ctx context.Context, directory string, source io.Reader, declaredSize uint64, expected *[sha256.Size]byte, prepare func(context.Context) error, use func(io.ReadSeeker, domain.UploadContentDigest) error, settle func(*os.File, bool)) (resultError error) {
	// 1. 检查输入、声明上限和取消状态。
	var file *os.File
	defer func() {
		cleaned := true
		if file != nil {
			var cleanupError error
			cleaned, cleanupError = cleanupUploadContentFile(file)
			resultError = errors.Join(resultError, cleanupError)
		}
		if settle != nil {
			settle(file, cleaned)
		}
	}()
	if err := validateUploadContentFileInput(ctx, directory, source, declaredSize, use); err != nil {
		return err
	}
	expected = copyOptionalValue(expected)

	// 2. 执行可选的准备步骤，成功且未取消后才能接收正文。
	if prepare != nil {
		prepareError := prepare(ctx)
		if err := ctx.Err(); err != nil {
			return errors.Join(prepareError, err)
		}
		if prepareError != nil {
			return prepareError
		}
	}

	// 3. 创建随机临时文件，安排关闭和删除。
	file, err := os.CreateTemp(directory, "upload-*.part")
	if err != nil {
		return fmt.Errorf("创建上传临时文件失败: %w", err)
	}

	// 4. 受限复制文件并校验预期摘要。
	digest, err := CopyUploadContent(ctx, file, source, declaredSize)
	if err != nil {
		return err
	}
	if err := ValidateUploadContentChecksum(digest.SHA256, expected); err != nil {
		return err
	}

	// 5. 同步文件并定位到开头。
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

	// 6. 调用文件使用方，复查取消并交付结果。
	useError := use(file, *digest)
	if err := ctx.Err(); err != nil {
		return errors.Join(useError, err)
	}
	return useError
}

// validateUploadContentFileInput 复用接收参数顺序，使预占前和创建前的检查一致。
func validateUploadContentFileInput(ctx context.Context, directory string, source io.Reader, declaredSize uint64, use func(io.ReadSeeker, domain.UploadContentDigest) error) error {
	if ctx == nil || directory == "" || source == nil || use == nil {
		return fmt.Errorf("接收上传临时文件需要上下文、临时目录、来源和文件使用方法")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if declaredSize > domain.MaxSingleUploadSizeBytes {
		return domain.ErrUploadFileTooLarge
	}
	return nil
}

// cleanupUploadContentFile 保留原始清理错误，并单独报告资源是否已经回收。
func cleanupUploadContentFile(file *os.File) (bool, error) {
	// 1. 先关闭再删除，任一失败都继续完成另一项尝试。
	closeError := file.Close()
	removeError := os.Remove(file.Name())
	cleaned := (closeError == nil || errors.Is(closeError, os.ErrClosed)) &&
		(removeError == nil || errors.Is(removeError, os.ErrNotExist))
	// 2. 已关闭或已不存在可以确认回收，原调用仍保留对应错误事实。
	var resultError error
	if closeError != nil {
		resultError = errors.Join(resultError, fmt.Errorf("关闭上传临时文件失败: %w", closeError))
	}
	if removeError != nil {
		resultError = errors.Join(resultError, fmt.Errorf("删除上传临时文件失败: %w", removeError))
	}
	return cleaned, resultError
}
