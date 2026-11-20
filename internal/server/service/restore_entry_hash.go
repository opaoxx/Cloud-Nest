package service

import (
	"crypto/sha256"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// BuildRestoreEntryRequestHash 为单项恢复生成版本化请求摘要。
// 失败时返回零值摘要，归属和实际版本在后续用户事务中核对。
func BuildRestoreEntryRequestHash(entryID, expectedRevision uint64) ([sha256.Size]byte, error) {
	// 1. 检查目标 ID 和预期版本是否非零。
	if entryID == 0 || expectedRevision == 0 {
		return [sha256.Size]byte{}, fmt.Errorf("生成恢复请求摘要需要非零目标 ID 和预期版本")
	}

	// 2. 按固定字段顺序编码操作名、目标和版本。
	encoded, err := encodeEntryRevisionRequest(domain.RestoreEntryOperation, entryID, expectedRevision)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("编码恢复请求摘要失败: %w", err)
	}

	// 3. 计算并返回 32 字节 SHA-256 摘要。
	return sha256.Sum256(encoded), nil
}
