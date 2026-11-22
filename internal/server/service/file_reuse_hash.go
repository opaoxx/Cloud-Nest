package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// maxFileReuseRequestSize 是秒传 size_bytes 契约允许的最大 JSON 安全整数。
const maxFileReuseRequestSize uint64 = 1<<53 - 1

// BuildFileReuseRequestHash 为秒传声明生成可稳定比较的请求摘要。
// 使用通过校验的展示名和完整内容摘要，重名后缀由首次创建流程确定。
func BuildFileReuseRequestHash(
	parentID uint64, requestedName string, sizeBytes uint64, digest [sha256.Size]byte,
) ([sha256.Size]byte, error) {
	// 1. 校验父目录、大小和展示名。
	name, err := validateFileReuseDeclaration(parentID, requestedName, sizeBytes)
	if err != nil {
		return [sha256.Size]byte{}, err
	}

	// 2. 按固定顺序编码操作名与四项请求参数。
	request := struct {
		Operation string `json:"operation"`
		ParentID  string `json:"parent_id"`
		Name      string `json:"name"`
		SizeBytes string `json:"size_bytes"`
		SHA256    string `json:"sha256"`
	}{
		Operation: domain.FileReuseOperation,
		ParentID:  strconv.FormatUint(parentID, 10),
		Name:      name,
		SizeBytes: strconv.FormatUint(sizeBytes, 10),
		SHA256:    hex.EncodeToString(digest[:]),
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("编码秒传请求摘要失败: %w", err)
	}

	// 3. 对编码结果计算 SHA-256。
	return sha256.Sum256(encoded), nil
}

// validateFileReuseDeclaration 让请求摘要与实际创建使用相同的声明规则。
func validateFileReuseDeclaration(parentID uint64, requestedName string, sizeBytes uint64) (string, error) {
	if parentID == 0 {
		return "", fmt.Errorf("秒传声明需要非零父目录 ID")
	}
	if sizeBytes > maxFileReuseRequestSize {
		return "", fmt.Errorf("秒传声明的文件大小超出 JSON 安全整数范围")
	}
	name, err := ValidateEntryName(requestedName)
	if err != nil {
		return "", err
	}
	// 同时检查实际用于同名查询的名称键能否存入数据库。
	if _, err := BuildEntryNameKey(name); err != nil {
		return "", err
	}
	return name, nil
}
