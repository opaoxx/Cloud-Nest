package storage

import (
	"context"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ListUploadParts 只读获取指定会话的完整分页观察结果，整轮共用一个读取期限。
// pageSize 控制内部查询每页的数量。中途失败时返回 nil，不交付部分回执。
func (client *Client) ListUploadParts(ctx context.Context, objectKey, uploadID string, pageSize int) ([]domain.UploadPartReceipt, error) {
	// 1. 设置整轮期限，从标记零开始读取同一会话。
	if ctx == nil {
		return nil, fmt.Errorf("完整读取分片回执需要上下文")
	}
	readContext, cancel := context.WithTimeout(ctx, uploadPartsReadTimeout)
	defer cancel()
	receipts := make([]domain.UploadPartReceipt, 0)
	var marker uint32
	for {
		// 2. 单页入口保证编号递增且有上限。已声明有续页却读到空页时，整轮失败。
		page, err := client.ListUploadPartsPage(readContext, objectKey, uploadID, marker, pageSize)
		if err != nil {
			return nil, err
		}
		if marker != 0 && len(page.Parts) == 0 {
			return nil, domain.ErrUploadStorageUnavailable
		}
		receipts = append(receipts, page.Parts...)

		// 3. 读到末页才交付完整结果，其余情况继续使用存储端标记。
		if page.NextMarker == 0 {
			if err := readContext.Err(); err != nil {
				return nil, err
			}
			return receipts, nil
		}
		marker = page.NextMarker
	}
}
