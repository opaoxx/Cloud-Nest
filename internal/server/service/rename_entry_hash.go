package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// BuildRenameEntryRequestHash 为有效重命名参数生成版本化的固定编码摘要。
// 展示名采用统一校验后的原写法，失败返回零值摘要及错误。
func BuildRenameEntryRequestHash(entryID, expectedRevision uint64, rawName string) ([sha256.Size]byte, error) {
	// 1. 检查目标 ID 和预期版本。
	if entryID == 0 || expectedRevision == 0 {
		return [sha256.Size]byte{}, fmt.Errorf("生成重命名请求摘要需要非零目标 ID 和预期版本")
	}

	// 2. 复用名称与名称键规则，取得有效展示名。
	name, err := ValidateEntryName(rawName)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	// 名称键检查同时覆盖生成结果的字节上限，使有效输入与核心服务一致。
	if _, err := BuildEntryNameKey(name); err != nil {
		return [sha256.Size]byte{}, err
	}

	// 3. 按固定字段顺序编码版本化重命名参数。
	request := struct {
		Operation        string `json:"operation"`
		EntryID          string `json:"entry_id"`
		ExpectedRevision string `json:"expected_revision"`
		Name             string `json:"name"`
	}{
		Operation:        domain.RenameEntryOperation,
		EntryID:          strconv.FormatUint(entryID, 10),
		ExpectedRevision: strconv.FormatUint(expectedRevision, 10),
		Name:             name,
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("编码重命名请求摘要失败: %w", err)
	}

	// 4. 计算并返回 32 字节 SHA-256 摘要。
	return sha256.Sum256(encoded), nil
}
