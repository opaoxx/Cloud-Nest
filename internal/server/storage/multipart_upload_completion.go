package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CompleteMultipartUpload 合并原会话中已核对的有序分片，返回存储确认的对象定位。
// 调用方在事务外执行。结果不明时应检查原键和会话，再决定后续操作。
func (client *Client) CompleteMultipartUpload(
	ctx context.Context, objectKey, uploadID string, fileSizeBytes, partSizeBytes uint64, partCount uint32, receipts []domain.UploadPartReceipt,
) (*domain.UploadObjectResult, error) {
	// 1. 校验原对象键、会话和有序分片回执。
	if ctx == nil || client == nil || client.sdkClient == nil || client.bucket == "" {
		return nil, fmt.Errorf("完成分片上传需要存储客户端和上下文")
	}
	completeContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := completeContext.Err(); err != nil {
		return nil, err
	}
	location := domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: client.bucket, ObjectKey: objectKey}
	if !validStoredObjectLocation(location) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if !domain.ValidMultipartUploadID(uploadID) || !domain.ValidMultipartUploadLayout(fileSizeBytes, partSizeBytes, partCount) || len(receipts) != int(partCount) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	parts := make([]minio.CompletePart, len(receipts))
	for index, receipt := range receipts {
		number := uint32(index + 1)
		size, err := domain.ExpectedMultipartUploadPartSize(fileSizeBytes, partSizeBytes, partCount, number)
		if err != nil || receipt.PartNumber != number || receipt.SizeBytes != size ||
			!domain.ValidUploadPartETag(receipt.ETag) || !domain.ValidUploadPartChecksumSHA256(receipt.ChecksumSHA256) {
			return nil, domain.ErrInvalidUploadTaskData
		}
		parts[index] = minio.CompletePart{PartNumber: int(number), ETag: receipt.ETag, ChecksumSHA256: receipt.ChecksumSHA256}
	}

	// 2. 用同一会话提交一次合并请求。内部 SDK 已配置单次请求。
	if err := completeContext.Err(); err != nil {
		return nil, err
	}
	core := minio.Core{Client: client.sdkClient}
	info, err := core.CompleteMultipartUpload(completeContext, client.bucket, objectKey, uploadID, parts, minio.PutObjectOptions{})

	// 3. 核对成功响应，保留明确拒绝和结果不明的信号。
	if contextError := completeContext.Err(); contextError != nil {
		return nil, errors.Join(domain.ErrMultipartUploadCompletionUnknown, contextError)
	}
	if err != nil {
		return nil, multipartUploadCompletionError(err)
	}
	if info.Bucket != client.bucket || info.Key != objectKey || !validStoredObjectHeaderValue(info.ETag) || strings.TrimSpace(info.ETag) == "" ||
		(info.VersionID != "" && !validStoredObjectHeaderValue(info.VersionID)) {
		return nil, domain.ErrMultipartUploadCompletionUnknown
	}
	// 完成响应提供定位。大小来自成功提交的固定分片计划，整文件校验还会读取实际字节。
	result := &domain.UploadObjectResult{SizeBytes: fileSizeBytes, StorageETag: &info.ETag}
	if info.VersionID != "" {
		result.StorageVersionID = &info.VersionID
	}
	return result, nil
}

func multipartUploadCompletionError(err error) error {
	response := minio.ToErrorResponse(err)
	if response.Code == "InvalidPart" && (response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusOK) {
		return domain.ErrMultipartUploadPartRejected
	}
	if response.Code == "NoSuchUpload" && (response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusOK) {
		return errors.Join(domain.ErrMultipartUploadCompletionUnknown, domain.ErrUploadPartSessionMissing)
	}
	if cause := storageObjectError(err, domain.ErrMultipartUploadCompletionUnknown); cause != domain.ErrMultipartUploadCompletionUnknown {
		return errors.Join(domain.ErrMultipartUploadCompletionUnknown, cause)
	}
	return domain.ErrMultipartUploadCompletionUnknown
}
