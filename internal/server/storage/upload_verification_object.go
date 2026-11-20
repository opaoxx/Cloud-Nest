package storage

import (
	"context"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// OpenUploadVerificationObject 打开由当前校验任务引用的 MinIO 对象流。
// 调用方使用持有租约的任务定位对象，保持上下文有效并在读取结束后关闭流。
func (client *Client) OpenUploadVerificationObject(ctx context.Context, object *domain.StoredObject) (io.ReadCloser, error) {
	// 1. 核对暂存对象的状态、存储位置与版本。
	if client == nil || client.sdkClient == nil || client.bucket == "" || ctx == nil {
		return nil, fmt.Errorf("打开上传校验对象需要存储客户端和上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if object == nil || object.ID == 0 || object.OwnerUserID == 0 || object.Status != domain.StoredObjectStatusVerifying ||
		object.Provider != "local_minio" || object.Bucket != client.bucket {
		return nil, domain.ErrInvalidStoredObjectData
	}
	location := domain.CreateStagedObjectInput{Provider: object.Provider, Bucket: object.Bucket, ObjectKey: object.ObjectKey}
	options, err := objectReadOptions(location, object.StorageVersionID)
	if err != nil {
		return nil, err
	}

	// 2. 发起 GET，检查返回的对象身份。
	// Core.GetObject 立即执行 GET，成功后直接交付 HTTP 正文流。
	body, info, headers, err := (minio.Core{Client: client.sdkClient}).GetObject(ctx, client.bucket, object.ObjectKey, options)
	if contextError := ctx.Err(); contextError != nil {
		if body != nil {
			_ = body.Close()
		}
		return nil, contextError
	}
	if err != nil {
		switch minio.ToErrorResponse(err).Code {
		case "NoSuchKey", "NoSuchVersion":
			return nil, domain.ErrUploadObjectMissing
		default:
			return nil, storageObjectReadError(err, domain.ErrUploadStorageUnavailable)
		}
	}
	// 版本功能开启前的 null 版本可以省略版本响应头，请求仍明确携带 versionId=null。
	if headers.Get("Content-Range") != "" || !matchesObjectReadVersion(options.VersionID, info.VersionID) ||
		(object.StorageETag != nil && info.ETag != *object.StorageETag) {
		_ = body.Close()
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 交付可关闭的文件流，保留取消与读取失败的原因。
	return &uploadVerificationObjectBody{ReadCloser: body, ctx: ctx}, nil
}

type uploadVerificationObjectBody struct {
	io.ReadCloser
	ctx context.Context
}

func (body *uploadVerificationObjectBody) Read(buffer []byte) (int, error) {
	if err := body.ctx.Err(); err != nil {
		return 0, err
	}
	count, err := body.ReadCloser.Read(buffer)
	if contextError := body.ctx.Err(); contextError != nil {
		return count, contextError
	}
	if err != nil && err != io.EOF {
		return count, storageObjectReadError(err, domain.ErrUploadStorageUnavailable)
	}
	return count, err
}

func (body *uploadVerificationObjectBody) Close() error {
	if err := body.ReadCloser.Close(); err != nil {
		return storageObjectReadError(err, domain.ErrUploadStorageUnavailable)
	}
	return nil
}
