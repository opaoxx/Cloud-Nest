package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// BuildTrashEntryRequestHash 根据单项回收参数生成版本化请求摘要。
// 失败时返回零值摘要，归属和实际版本比对由后续事务完成。
func BuildTrashEntryRequestHash(entryID, expectedRevision uint64) ([sha256.Size]byte, error) {
	// 1. 检查目标 ID 和预期版本均为非零。
	if entryID == 0 || expectedRevision == 0 {
		return [sha256.Size]byte{}, fmt.Errorf("生成回收请求摘要需要非零目标 ID 和预期版本")
	}

	// 2. 将操作名、目标 ID 和版本按固定顺序编码。
	request := struct {
		Operation        string `json:"operation"`
		EntryID          string `json:"entry_id"`
		ExpectedRevision string `json:"expected_revision"`
	}{
		Operation:        domain.TrashEntryOperation,
		EntryID:          strconv.FormatUint(entryID, 10),
		ExpectedRevision: strconv.FormatUint(expectedRevision, 10),
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("编码回收请求摘要失败: %w", err)
	}

	// 3. 计算并返回 SHA-256 摘要。
	return sha256.Sum256(encoded), nil
}
