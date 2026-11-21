package storage

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const (
	uploadPartURLLifetime    = 5 * time.Minute
	uploadPartChecksumHeader = "x-amz-checksum-sha256"
	maxMinIOUploadPartBytes  = 5 << 30
)

// SignUploadPartURL 为当前桶下的一片内容签名。调用方提供已授权任务的固定对象键及会话。
// checksumSHA256 是对应分片的标准 Base64 摘要，taskExpiresAt 是当前任务截止时间。
func (client *Client) SignUploadPartURL(ctx context.Context, objectKey, uploadID string, partNumber uint32, sizeBytes uint64, checksumSHA256 string, taskExpiresAt time.Time) (*domain.SignedUploadPartURL, error) {
	// 1. 核对存储位置、分片声明和整秒有效期。
	if ctx == nil || client == nil || client.publicSigner == nil || client.bucket == "" {
		return nil, fmt.Errorf("签发分片上传地址需要存储签名客户端和上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	location := domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: client.bucket, ObjectKey: objectKey}
	if !validStoredObjectLocation(location) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if partNumber == 0 || partNumber > domain.MaxUploadPartCount {
		return nil, domain.ErrInvalidUploadPartNumber
	}
	if !domain.ValidMultipartUploadID(uploadID) || sizeBytes == 0 || sizeBytes > maxMinIOUploadPartBytes ||
		!domain.ValidUploadPartChecksumSHA256(checksumSHA256) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	lifetime := min(uploadPartURLLifetime, time.Until(taskExpiresAt)).Truncate(time.Second)
	if lifetime < time.Second {
		return nil, domain.ErrUploadTaskNotReady
	}

	// 2. 绑定会话、编号和必要请求头，用客户端访问地址签发 PUT。
	number := strconv.FormatUint(uint64(partNumber), 10)
	parameters := url.Values{"uploadId": {uploadID}, "partNumber": {number}}
	headers := make(http.Header)
	headers.Set("Content-Length", strconv.FormatUint(sizeBytes, 10))
	headers.Set(uploadPartChecksumHeader, checksumSHA256)
	signed, err := client.publicSigner.PresignHeader(ctx, http.MethodPut, client.bucket, objectKey, lifetime, parameters, headers)
	if contextError := ctx.Err(); contextError != nil {
		return nil, contextError
	}
	if err != nil {
		return nil, storageObjectError(err, domain.ErrUploadStorageUnavailable)
	}

	// 3. 核对签名及实际截止时间，防止签名时跨秒导致 URL 超过任务期限。
	query := signed.Query()
	if signed.Path != "/"+client.bucket+"/"+objectKey ||
		query.Get("uploadId") != uploadID || query.Get("partNumber") != number ||
		query.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" ||
		query.Get("X-Amz-SignedHeaders") != "content-length;host;x-amz-checksum-sha256" ||
		query.Get("X-Amz-Expires") != strconv.FormatInt(int64(lifetime/time.Second), 10) {
		return nil, domain.ErrUploadStorageUnavailable
	}
	signedAt, err := time.Parse(sigV4SigningTimeLayout, query.Get("X-Amz-Date"))
	if err != nil {
		return nil, domain.ErrUploadStorageUnavailable
	}
	expiresAt := signedAt.Add(lifetime)
	if expiresAt.After(taskExpiresAt) || !expiresAt.After(time.Now()) {
		return nil, domain.ErrUploadTaskNotReady
	}
	return &domain.SignedUploadPartURL{URL: signed.String(), ExpiresAt: expiresAt}, nil
}
