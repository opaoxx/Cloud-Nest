package storage

import (
	"context"
	"errors"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CheckMultipartUploadSessionExists 只读核对当前桶内的完整对象键和会话 ID。
// false 且无错误表示存储明确报告会话缺失。
func (client *Client) CheckMultipartUploadSessionExists(ctx context.Context, objectKey, uploadID string) (bool, error) {
	// 1. 只查第一页的一条分片，沿用桶、对象键和会话 ID 的响应校验。
	_, err := client.listUploadPartsPage(ctx, objectKey, uploadID, 0, 1, true)

	// 2. 合规的空分片页也证明会话存在。只有明确的缺失响应返回 false。
	if errors.Is(err, domain.ErrUploadPartSessionMissing) {
		return false, nil
	}
	return err == nil, err
}
