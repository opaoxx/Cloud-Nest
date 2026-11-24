package client

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

// ReadUploadParts reads the complete storage receipt list for the existing task.
func ReadUploadParts(ctx context.Context, address, accessToken, taskID string, plan MultipartPlan) ([]UploadPartReceipt, error) {
	// 1. 清单包含最多 10000 条回执，使用独立的有界响应长度。
	if !validEntryID(taskID) {
		return nil, errors.New("无效的上传任务 ID")
	}
	var result struct {
		Data struct {
			PartSizeBytes int64 `json:"part_size_bytes"`
			PartCount     int   `json:"part_count"`
			Parts         []struct {
				Number         int    `json:"part_number"`
				SizeBytes      int64  `json:"size_bytes"`
				ChecksumSHA256 string `json:"checksum_sha256"`
				ETag           string `json:"etag"`
			} `json:"parts"`
		} `json:"data"`
	}
	const maxPartListBytes = 4 * 1024 * 1024
	if err := getAuthenticatedJSONWithLimit(ctx, address, accessToken, "uploads/"+taskID+"/parts", nil, &result, maxPartListBytes); err != nil {
		if errors.Is(err, ErrAccessTokenExpired) {
			return nil, err
		}
		return nil, errors.New("无法读取存储分片清单，请稍后重试")
	}
	data := result.Data
	if data.PartSizeBytes != plan.PartSizeBytes || data.PartCount != plan.PartCount || data.Parts == nil || len(data.Parts) > plan.PartCount {
		return nil, errors.New("存储分片清单与本机计划不一致")
	}

	// 2. 校验整个清单的结构，将 Base64 摘要统一转成 SQLite 使用的十六进制。
	receipts := make([]UploadPartReceipt, 0, len(data.Parts))
	previous := 0
	for _, part := range data.Parts {
		checksum, err := base64.StdEncoding.Strict().DecodeString(part.ChecksumSHA256)
		if err != nil || len(checksum) != sha256.Size || len(part.ChecksumSHA256) != 44 ||
			part.Number <= previous || part.Number > plan.PartCount || part.SizeBytes < 1 || part.SizeBytes > maxJSONSafeInteger ||
			!uploadPartETagPattern.MatchString(part.ETag) {
			return nil, errors.New("存储返回了无效的分片回执")
		}
		receipts = append(receipts, UploadPartReceipt{
			Number: part.Number, SizeBytes: part.SizeBytes, SHA256: hex.EncodeToString(checksum), ETag: part.ETag,
		})
		previous = part.Number
	}
	return receipts, nil
}
