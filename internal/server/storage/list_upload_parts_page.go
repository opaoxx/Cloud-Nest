package storage

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const (
	maxUploadPartsPageSize = 1000
	uploadPartsReadTimeout = 5 * time.Second
)

var uploadPartETagPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// ListUploadPartsPage 只读查询当前桶中指定对象和会话的一页分片回执。
// marker 为零时从头读取。成功结果保留存储报告的片长，供调用方按任务计划核对。
func (client *Client) ListUploadPartsPage(ctx context.Context, objectKey, uploadID string, marker uint32, limit int) (*domain.UploadPartReceiptPage, error) {
	// 1. 校验请求和存储位置，使用内部客户端只读获取一页。
	if ctx == nil || client == nil || client.sdkClient == nil || client.bucket == "" {
		return nil, fmt.Errorf("读取分片回执需要客户端和上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	location := domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: client.bucket, ObjectKey: objectKey}
	if !validStoredObjectLocation(location) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if !domain.ValidMultipartUploadID(uploadID) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if marker > domain.MaxUploadPartCount || limit < 1 || limit > maxUploadPartsPageSize {
		return nil, fmt.Errorf("分片回执标记或每页数量超出范围")
	}
	readContext, cancel := context.WithTimeout(ctx, uploadPartsReadTimeout)
	defer cancel()
	core := minio.Core{Client: client.sdkClient}
	page, err := core.ListObjectParts(readContext, client.bucket, objectKey, uploadID, int(marker), limit)
	if contextError := readContext.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchUpload" {
			return nil, domain.ErrUploadPartSessionMissing
		}
		return nil, storageObjectError(err, domain.ErrUploadStorageUnavailable)
	}

	// 2. 核对返回的桶、对象、会话及标记，将严格升序的分片转成回执。
	if page.Bucket != client.bucket || page.Key != objectKey || page.UploadID != uploadID ||
		page.PartNumberMarker != int(marker) || len(page.ObjectParts) > limit {
		return nil, domain.ErrUploadStorageUnavailable
	}
	result := &domain.UploadPartReceiptPage{Parts: make([]domain.UploadPartReceipt, 0, len(page.ObjectParts))}
	lastPartNumber := int(marker)
	for _, part := range page.ObjectParts {
		etag := part.ETag
		if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' {
			etag = etag[1 : len(etag)-1]
		}
		if part.PartNumber <= lastPartNumber || part.PartNumber > domain.MaxUploadPartCount || part.Size < 0 ||
			!uploadPartETagPattern.MatchString(etag) || !domain.ValidUploadPartChecksumSHA256(part.ChecksumSHA256) {
			return nil, domain.ErrUploadStorageUnavailable
		}
		result.Parts = append(result.Parts, domain.UploadPartReceipt{
			PartNumber: uint32(part.PartNumber), SizeBytes: uint64(part.Size), ETag: etag, ChecksumSHA256: part.ChecksumSHA256,
		})
		lastPartNumber = part.PartNumber
	}

	// 3. 截断页必须给出可继续的标记，最后一页统一返回零。
	if page.IsTruncated {
		if lastPartNumber <= int(marker) || lastPartNumber >= domain.MaxUploadPartCount || page.NextPartNumberMarker != lastPartNumber {
			return nil, domain.ErrUploadStorageUnavailable
		}
		result.NextMarker = uint32(lastPartNumber)
	}
	if err := readContext.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
