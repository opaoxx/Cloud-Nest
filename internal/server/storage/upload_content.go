package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"unicode"

	"github.com/minio/minio-go/v7"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// PutUploadContent 将已校验的完整临时文件条件写入当前客户端的桶。
// 文件应在调用期间保持稳定，生命周期由调用方管理。
func (client *Client) PutUploadContent(ctx context.Context, objectKey string, source io.ReadSeeker, sizeBytes uint64) (*domain.UploadObjectResult, error) {
	// 1. 检查客户端、上下文、对象键和单文件大小上限。
	if client == nil || client.sdkClient == nil || client.bucket == "" || ctx == nil || source == nil {
		return nil, fmt.Errorf("写入上传暂存对象需要客户端、上下文和文件")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	location := domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: client.bucket, ObjectKey: objectKey}
	if !location.IsValid() || strings.TrimSpace(objectKey) == "" || strings.ContainsFunc(objectKey, unicode.IsControl) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if sizeBytes > domain.MaxSingleUploadSizeBytes {
		return nil, domain.ErrUploadFileTooLarge
	}

	// 2. 核对临时文件大小，并定位到开头。
	fileSize, err := source.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("检查上传临时文件大小失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fileSize != int64(sizeBytes) {
		return nil, domain.ErrUploadBodySizeMismatch
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("定位上传临时文件失败: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 3. 按单次 PUT 和条件创建提交文件。
	options := minio.PutObjectOptions{
		ContentType:      "application/octet-stream",
		DisableMultipart: true,
		SendContentMd5:   true,
	}
	options.SetMatchETagExcept("*")
	info, err := client.sdkClient.PutObject(ctx, client.bucket, objectKey, source, int64(sizeBytes), options)

	// 4. 处理取消、存储故障和对象已存在。
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		if response := minio.ToErrorResponse(err); response.StatusCode == http.StatusPreconditionFailed || response.Code == "PreconditionFailed" {
			return nil, domain.ErrUploadObjectExists
		}
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return nil, fmt.Errorf("上传暂存对象超时: %w", context.DeadlineExceeded)
		}
		return nil, domain.ErrUploadStorageUnavailable
	}

	// 5. 返回可保存的存储确认信息。
	for _, value := range []string{info.ETag, info.VersionID} {
		if len(value) > 255 {
			return nil, domain.ErrUploadStorageUnavailable
		}
		for _, character := range value {
			if character > 127 {
				return nil, domain.ErrUploadStorageUnavailable
			}
		}
	}
	result := &domain.UploadObjectResult{SizeBytes: sizeBytes}
	if info.ETag != "" {
		result.StorageETag = &info.ETag
	}
	if info.VersionID != "" {
		result.StorageVersionID = &info.VersionID
	}
	return result, nil
}
