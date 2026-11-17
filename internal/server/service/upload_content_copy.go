package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CopyUploadContent 受限复制本次文件内容并计算完整 SHA256，成功时大小与声明一致。
// 输入输出流的生命周期及失败后的目标清理由调用方管理。
// 取消在 I/O 边界生效，阻塞调用由上层上下文感知实现或读写超时解除。
func CopyUploadContent(ctx context.Context, destination io.Writer, source io.Reader, declaredSize uint64) (*domain.UploadContentDigest, error) {
	// 1. 检查上下文、输入输出流和声明大小上限。
	if ctx == nil || destination == nil || source == nil {
		return nil, fmt.Errorf("复制上传内容需要上下文、目标写入器和源读取器")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if declaredSize > domain.MaxSingleUploadSizeBytes {
		return nil, domain.ErrUploadFileTooLarge
	}

	// 2. 准备受限读取与 SHA256 写入链。
	reader := &uploadContentReader{ctx: ctx, source: source}
	hasher := sha256.New()
	writer := io.MultiWriter(uploadContentWriter{ctx: ctx, destination: destination}, hasher)
	buffer := make([]byte, 32*1024)

	// 3. 流式复制声明范围内的内容，检查取消和读写错误。
	written, copyError := io.CopyBuffer(writer, io.LimitReader(reader, int64(declaredSize)), buffer)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if copyError != nil {
		return nil, fmt.Errorf("复制上传内容失败: %w", copyError)
	}
	if uint64(written) != declaredSize {
		return nil, domain.ErrUploadBodySizeMismatch
	}

	// 4. 探测额外字节，核对实际大小。
	var extraByte [1]byte
	extraCount, readError := io.ReadFull(reader, extraByte[:])
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if extraCount != 0 {
		return nil, domain.ErrUploadBodySizeMismatch
	}
	if readError != io.EOF {
		return nil, fmt.Errorf("读取上传结束检测字节失败: %w", readError)
	}

	// 5. 返回完整大小和摘要，交由调用方管理暂存目标。
	result := &domain.UploadContentDigest{SizeBytes: uint64(written)}
	copy(result.SHA256[:], hasher.Sum(nil))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// uploadContentReader 在每次读取前后检查取消，并识别持续无进展的读取器。
type uploadContentReader struct {
	// ctx 保存本次内容接收的取消与期限。
	ctx context.Context
	// source 是调用方提供的正文来源。
	source io.Reader
	// emptyReads 统计连续返回零字节且无错误的读取次数。
	emptyReads int
}

func (reader *uploadContentReader) Read(buffer []byte) (int, error) {
	// 1. 取消时结束读取，完成底层读取后再复查。
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := reader.source.Read(buffer)
	if contextError := reader.ctx.Err(); contextError != nil {
		return count, contextError
	}
	// 2. 连续一百次空读返回标准无进展错误，正常进展时重置计数。
	if count == 0 && err == nil {
		reader.emptyReads++
		if reader.emptyReads >= 100 {
			return 0, io.ErrNoProgress
		}
	} else {
		reader.emptyReads = 0
	}
	return count, err
}

// uploadContentWriter 在写入边界复查取消，底层已写入内容由调用方清理。
type uploadContentWriter struct {
	// ctx 保存本次内容接收的取消与期限。
	ctx context.Context
	// destination 是调用方管理的暂存目标。
	destination io.Writer
}

func (writer uploadContentWriter) Write(content []byte) (int, error) {
	// 1. 取消后结束写入，底层返回后再次检查。
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := writer.destination.Write(content)
	if contextError := writer.ctx.Err(); contextError != nil {
		return count, contextError
	}
	return count, err
}
