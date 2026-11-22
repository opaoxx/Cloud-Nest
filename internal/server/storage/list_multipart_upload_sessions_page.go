package storage

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ListMultipartUploadSessionsPage 只读查询当前桶中指定应用对象键的一页会话。
// 固定 MinIO 以完整对象键处理非空 prefix。空 marker 表示从头读取。
func (client *Client) ListMultipartUploadSessionsPage(
	ctx context.Context, objectKey, marker string, limit int,
) (*domain.MultipartUploadSessionPage, error) {
	// 1. 校验应用对象键、会话游标和页大小。
	if ctx == nil || client == nil || client.sdkClient == nil || client.bucket == "" {
		return nil, fmt.Errorf("读取分片会话需要客户端和上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, valid := domain.ParseUploadObjectKey(objectKey); !valid {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if marker != "" && !domain.ValidMultipartUploadID(marker) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if limit < 1 || limit > domain.MaxMultipartUploadSessionsPageSize {
		return nil, fmt.Errorf("分片会话每页数量超出范围")
	}

	// 2. 只读获取该对象的一页会话。后续页同时携带原对象键和会话游标。
	keyMarker := ""
	if marker != "" {
		keyMarker = objectKey
	}
	readContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	core := minio.Core{Client: client.sdkClient}
	page, err := core.ListMultipartUploads(readContext, client.bucket, objectKey, keyMarker, marker, "", limit)
	if contextError := readContext.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, storageObjectError(err, domain.ErrUploadStorageUnavailable)
	}

	// 3. 核对范围、发起时间和下一页游标，原样交付会话 ID。
	if !matchesMultipartUploadSessionPage(page, client.bucket, objectKey, keyMarker, marker) || len(page.Uploads) > limit {
		return nil, domain.ErrUploadStorageUnavailable
	}
	result := &domain.MultipartUploadSessionPage{Sessions: make([]domain.MultipartUploadSession, 0, len(page.Uploads))}
	seen := make(map[string]struct{}, len(page.Uploads))
	var previousTime time.Time
	for _, upload := range page.Uploads {
		_, duplicate := seen[upload.UploadID]
		if upload.Err != nil || upload.Key != objectKey || !domain.ValidMultipartUploadID(upload.UploadID) ||
			upload.UploadID == marker || duplicate || upload.Initiated.IsZero() || upload.Initiated.Before(previousTime) {
			return nil, domain.ErrUploadStorageUnavailable
		}
		seen[upload.UploadID] = struct{}{}
		previousTime = upload.Initiated
		result.Sessions = append(result.Sessions, domain.MultipartUploadSession{
			ObjectKey: objectKey, UploadID: upload.UploadID, InitiatedAt: upload.Initiated.UTC(),
		})
	}
	if page.IsTruncated {
		if len(result.Sessions) == 0 || page.NextUploadIDMarker != result.Sessions[len(result.Sessions)-1].UploadID {
			return nil, domain.ErrUploadStorageUnavailable
		}
		result.NextUploadID = page.NextUploadIDMarker
	}
	if err := readContext.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func matchesMultipartUploadSessionPage(page minio.ListMultipartUploadsResult, bucket, objectKey, keyMarker, marker string) bool {
	// SDK 已解码会话的键和下一页标记，当前页的 prefix 和 key-marker 仍使用响应原文。
	prefix, echoedKeyMarker := page.Prefix, page.KeyMarker
	switch page.EncodingType {
	case "":
	case "url":
		var err error
		prefix, err = url.QueryUnescape(prefix)
		if err != nil {
			return false
		}
		echoedKeyMarker, err = url.QueryUnescape(echoedKeyMarker)
		if err != nil {
			return false
		}
	default:
		return false
	}
	return page.Bucket == bucket && prefix == objectKey && echoedKeyMarker == keyMarker &&
		(page.UploadIDMarker == "" || page.UploadIDMarker == marker) &&
		(page.NextKeyMarker == "" || page.NextKeyMarker == objectKey) &&
		page.Delimiter == "" && len(page.CommonPrefixes) == 0
}
