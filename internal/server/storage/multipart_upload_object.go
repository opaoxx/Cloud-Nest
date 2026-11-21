package storage

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// StatMultipartUploadObject 只读查询配置桶中原暂存键的当前对象元数据。
// objectKey 来自已核对的任务目标。返回值只提供对象事实，任务大小、内容及结果写回由调用方复核。
func (client *Client) StatMultipartUploadObject(ctx context.Context, objectKey string) (*domain.UploadObjectResult, error) {
	// 1. 校验原对象键，并设置查询期限。
	if ctx == nil || client == nil || client.sdkClient == nil || client.bucket == "" {
		return nil, fmt.Errorf("查询分片对象需要存储客户端和上下文")
	}
	checkContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := checkContext.Err(); err != nil {
		return nil, err
	}
	location := domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: client.bucket, ObjectKey: objectKey}
	options, err := objectReadOptions(location, nil)
	if err != nil {
		return nil, err
	}

	// 2. 只读查询配置桶中的对象，区分缺失和查询失败。
	info, err := client.sdkClient.StatObject(checkContext, client.bucket, objectKey, options)
	if contextError := checkContext.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		response := minio.ToErrorResponse(err)
		// SDK 会把普通 404 映射为 NoSuchKey，要求有存储请求 ID。删除标记可能遮住历史版本。
		if response.StatusCode == http.StatusNotFound && response.Code == "NoSuchKey" && response.RequestID != "" &&
			!info.IsDeleteMarker && info.VersionID == "" {
			return nil, domain.ErrUploadObjectMissing
		}
		return nil, storageObjectError(err, domain.ErrUploadStorageUnavailable)
	}

	// 3. 核对元数据，返回大小、ETag 和版本定位。
	if info.IsDeleteMarker || info.Size < 0 || info.Headers.Get("Content-Range") != "" ||
		!validStoredObjectHeaderValue(info.ETag) || strings.TrimSpace(info.ETag) == "" ||
		(info.VersionID != "" && !validStoredObjectHeaderValue(info.VersionID)) {
		return nil, domain.ErrUploadStorageUnavailable
	}
	result := &domain.UploadObjectResult{SizeBytes: uint64(info.Size), StorageETag: &info.ETag}
	if info.VersionID != "" {
		result.StorageVersionID = &info.VersionID
	}
	return result, nil
}
