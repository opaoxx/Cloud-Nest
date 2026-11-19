package service

import (
	"context"
	"io"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ReadUploadVerificationDigest 读取待校验的原始对象流，返回实际大小和 SHA-256。
// 调用方负责关闭对象流；大小不符、读取故障及取消均返回空结果。
func ReadUploadVerificationDigest(ctx context.Context, source io.Reader, declaredSize uint64) (*domain.UploadContentDigest, error) {
	// 1. 复用受限读取和摘要计算，丢弃复制输出。
	return CopyUploadContent(ctx, io.Discard, source, declaredSize)
}
