package storage

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// DeleteStoredObject 删除已提交删除意图的 MinIO 对象或指定版本。
// 调用方在数据库事务外执行，成功或确认目标已不存在时返回 nil。
func (client *Client) DeleteStoredObject(ctx context.Context, object *domain.StoredObject) error {
	// 1. 校验对象状态、存储桶、对象键和版本。
	if ctx == nil || client == nil || client.sdkClient == nil || client.bucket == "" {
		return fmt.Errorf("删除存储对象需要客户端和上下文")
	}
	deleteContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := deleteContext.Err(); err != nil {
		return err
	}
	if object == nil || object.ID == 0 || object.OwnerUserID == 0 || object.Status != domain.StoredObjectStatusDeleting ||
		object.Provider != "local_minio" || object.Bucket != client.bucket {
		return domain.ErrInvalidStoredObjectData
	}
	location := domain.CreateStagedObjectInput{Provider: object.Provider, Bucket: object.Bucket, ObjectKey: object.ObjectKey}
	options, err := objectReadOptions(location, object.StorageVersionID)
	if err != nil {
		return err
	}

	// 2. 确定目标版本，向 MinIO 发起单对象删除。
	versionID := options.VersionID
	if versionID == "" {
		info, err := client.sdkClient.StatObject(deleteContext, client.bucket, object.ObjectKey, options)
		if contextError := deleteContext.Err(); contextError != nil {
			return contextError
		}
		if info.IsDeleteMarker || (info.VersionID != "" && info.VersionID != "null") {
			return domain.ErrInvalidStoredObjectData
		}
		if err != nil {
			if storedObjectDeleteTargetMissing(err) {
				return nil
			}
			return storageObjectError(err, domain.ErrStoredObjectDeleteUnavailable)
		}
		// 明确针对 null 版本，避免桶启用版本后写入删除标记。
		versionID = "null"
	}
	err = client.sdkClient.RemoveObject(deleteContext, client.bucket, object.ObjectKey, minio.RemoveObjectOptions{VersionID: versionID})

	// 3. 分别处理成功、目标已不存在、取消和存储故障。
	if contextError := deleteContext.Err(); contextError != nil {
		return contextError
	}
	if err == nil || storedObjectDeleteTargetMissing(err) {
		return nil
	}
	return storageObjectError(err, domain.ErrStoredObjectDeleteUnavailable)
}

// storedObjectDeleteTargetMissing 只接受能归因于存储目标的缺失响应。
func storedObjectDeleteTargetMissing(err error) bool {
	response := minio.ToErrorResponse(err)
	// SDK 会将无结构的 404 转成 NoSuchKey，要求响应带有存储服务请求 ID。
	return response.StatusCode == http.StatusNotFound && response.RequestID != "" &&
		(response.Code == "NoSuchKey" || response.Code == "NoSuchVersion")
}
