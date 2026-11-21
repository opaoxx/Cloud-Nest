package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CreateMultipartUpload 为当前桶的既定对象键创建分片会话，并返回原始存储会话 ID。
// 调用方在数据库事务外执行，并在后续事务中绑定结果。
func (client *Client) CreateMultipartUpload(ctx context.Context, objectKey string) (string, error) {
	// 1. 校验客户端、目标对象键和上下文，限制本次存储调用时间。
	if ctx == nil || client == nil || client.sdkClient == nil || client.bucket == "" {
		return "", fmt.Errorf("创建分片会话需要客户端和上下文")
	}
	createContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := createContext.Err(); err != nil {
		return "", err
	}
	location := domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: client.bucket, ObjectKey: objectKey}
	if !validStoredObjectLocation(location) {
		return "", domain.ErrInvalidStoredObjectData
	}

	// 2. 复用内部 SDK，按已验收的 SHA-256 分片契约创建会话。
	core := minio.Core{Client: client.sdkClient}
	uploadID, err := core.NewMultipartUpload(createContext, client.bucket, objectKey, minio.PutObjectOptions{
		ContentType:  "application/octet-stream",
		UserMetadata: map[string]string{"x-amz-checksum-algorithm": "SHA256"},
	})

	// 3. 保留取消和超时，隐藏原始存储错误，并核对会话 ID。
	if contextError := createContext.Err(); contextError != nil {
		return "", contextError
	}
	if err != nil {
		return "", storageObjectError(err, domain.ErrUploadStorageUnavailable)
	}
	if !domain.ValidMultipartUploadID(uploadID) {
		return "", domain.ErrUploadStorageUnavailable
	}
	return uploadID, nil
}
