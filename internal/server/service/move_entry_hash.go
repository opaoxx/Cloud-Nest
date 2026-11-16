package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// BuildMoveEntryRequestHash 为移动参数生成版本化固定编码摘要，失败返回零值摘要。
// 源与目标的可见性和关系由移动核心服务在用户事务中检查。
func BuildMoveEntryRequestHash(entryID, expectedRevision, targetParentID uint64) ([sha256.Size]byte, error) {
	// 1. 检查源 ID、预期版本和目标父目录 ID。
	if entryID == 0 || expectedRevision == 0 || targetParentID == 0 {
		return [sha256.Size]byte{}, fmt.Errorf("生成移动请求摘要需要非零源 ID、预期版本和目标父目录 ID")
	}

	// 2. 按固定字段顺序编码版本化移动参数。
	request := struct {
		Operation        string `json:"operation"`
		EntryID          string `json:"entry_id"`
		ExpectedRevision string `json:"expected_revision"`
		TargetParentID   string `json:"target_parent_id"`
	}{
		Operation:        domain.MoveEntryOperation,
		EntryID:          strconv.FormatUint(entryID, 10),
		ExpectedRevision: strconv.FormatUint(expectedRevision, 10),
		TargetParentID:   strconv.FormatUint(targetParentID, 10),
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("编码移动请求摘要失败: %w", err)
	}

	// 3. 计算并返回 32 字节 SHA-256 摘要。
	return sha256.Sum256(encoded), nil
}
