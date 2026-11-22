package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// AbortMultipartUpload 向当前桶中指定原对象键和会话 ID 发送中止请求。
// nil 表示收到存储确认，ErrUploadPartSessionMissing 表示存储明确报告会话缺失。
// 两种结果均需由调用方在事务外复查原会话和原对象，再判断收尾条件。
func (client *Client) AbortMultipartUpload(ctx context.Context, objectKey, uploadID string) error {
	// 1. 核对当前桶、原对象键和会话 ID，保留二者的原值并限制调用时间。
	if ctx == nil || client == nil || client.sdkClient == nil || client.bucket == "" {
		return fmt.Errorf("中止分片上传需要存储客户端和上下文")
	}
	abortContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := abortContext.Err(); err != nil {
		return err
	}
	location := domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: client.bucket, ObjectKey: objectKey}
	if !validStoredObjectLocation(location) {
		return domain.ErrInvalidStoredObjectData
	}
	if !domain.ValidMultipartUploadID(uploadID) {
		return domain.ErrInvalidUploadTaskData
	}

	// 2. 使用内部 SDK 发出单次中止请求，uploadId 将操作限制在指定会话。
	core := minio.Core{Client: client.sdkClient}
	err := core.AbortMultipartUpload(abortContext, client.bucket, objectKey, uploadID)

	// 3. 区分成功、存储明确报告会话缺失和仍需复查的结果，隐藏底层响应。
	if contextError := abortContext.Err(); contextError != nil {
		return errors.Join(domain.ErrUploadStorageUnavailable, contextError)
	}
	if err == nil {
		return nil
	}
	response := minio.ToErrorResponse(err)
	if response.StatusCode == http.StatusNotFound && response.Code == "NoSuchUpload" && response.RequestID != "" {
		return domain.ErrUploadPartSessionMissing
	}
	cause := storageObjectError(err, domain.ErrUploadStorageUnavailable)
	if cause != domain.ErrUploadStorageUnavailable {
		return errors.Join(domain.ErrUploadStorageUnavailable, cause)
	}
	return cause
}
