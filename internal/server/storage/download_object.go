package storage

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CheckDownloadObject 只读核对下载目标指定的 MinIO 对象版本与大小。
// 调用方提供从可信快照取得的下载目标，检查最多五秒并服从调用方期限。
func (client *Client) CheckDownloadObject(ctx context.Context, target *domain.EntryDownloadTarget) error {
	// 1. 检查对象位置、版本和请求期限。
	if ctx == nil || client == nil || client.sdkClient == nil || client.bucket == "" {
		return fmt.Errorf("检查下载对象需要存储客户端和上下文")
	}
	checkContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := checkContext.Err(); err != nil {
		return err
	}
	options, err := client.downloadObjectOptions(target)
	if err != nil {
		return err
	}

	// 2. 向 MinIO 查询指定版本的元数据。
	info, err := client.sdkClient.StatObject(checkContext, client.bucket, target.ObjectKey, options)
	if contextError := checkContext.Err(); contextError != nil {
		return contextError
	}
	if err != nil {
		response := minio.ToErrorResponse(err)
		if response.Code == "NoSuchKey" || response.Code == "NoSuchVersion" ||
			(info.IsDeleteMarker && (response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed)) {
			return domain.ErrDownloadTargetUnavailable
		}
		return storageObjectError(err, domain.ErrDownloadStorageUnavailable)
	}

	// 3. 核对版本和大小，返回检查结果。
	if info.IsDeleteMarker || !matchesObjectReadVersion(options.VersionID, info.VersionID) {
		return domain.ErrDownloadTargetUnavailable
	}
	if info.Size < 0 || info.Headers.Get("Content-Range") != "" {
		return domain.ErrDownloadStorageUnavailable
	}
	if uint64(info.Size) != target.SizeBytes {
		return domain.ErrDownloadTargetUnavailable
	}
	return nil
}

// downloadObjectOptions 让元数据检查和下载签名使用相同的对象定位规则。
func (client *Client) downloadObjectOptions(target *domain.EntryDownloadTarget) (minio.GetObjectOptions, error) {
	// 1. 核对对象引用及客户端负责的存储范围。
	if target == nil || target.ObjectID == 0 || target.UserID == 0 || target.Provider != "local_minio" || target.Bucket != client.bucket {
		return minio.GetObjectOptions{}, domain.ErrInvalidStoredObjectData
	}
	// 2. 校验实际对象键和可空版本。
	location := domain.CreateStagedObjectInput{Provider: target.Provider, Bucket: target.Bucket, ObjectKey: target.ObjectKey}
	return objectReadOptions(location, target.StorageVersionID)
}
