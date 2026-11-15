package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// BuildCreateDirectoryRequestHash 为有效创建参数生成版本化的固定编码摘要。
// 展示名采用统一校验后的原写法，颜色保留 nil/空串，失败返回零值摘要及错误。
func BuildCreateDirectoryRequestHash(parentID uint64, rawName string, colorKey *string) ([sha256.Size]byte, error) {
	// 1. 检查父目录 ID。
	if parentID == 0 {
		return [sha256.Size]byte{}, fmt.Errorf("生成创建文件夹请求摘要需要非零父目录 ID")
	}

	// 2. 复用创建规则校验名称和颜色，取得规范化输入。
	name, err := ValidateEntryName(rawName)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	// 名称键检查同时覆盖生成结果的字节上限，使有效输入与创建服务一致。
	if _, err := BuildEntryNameKey(name); err != nil {
		return [sha256.Size]byte{}, err
	}
	if !domain.ValidDirectoryColorKey(colorKey) {
		return [sha256.Size]byte{}, domain.ErrInvalidDirectoryColorKey
	}

	// 3. 按固定字段顺序编码版本化请求参数。
	request := struct {
		Operation string  `json:"operation"`
		ParentID  string  `json:"parent_id"`
		Name      string  `json:"name"`
		ColorKey  *string `json:"color_key"`
	}{
		Operation: domain.CreateDirectoryOperation,
		ParentID:  strconv.FormatUint(parentID, 10),
		Name:      name,
		ColorKey:  copyOptionalValue(colorKey),
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("编码创建文件夹请求摘要失败: %w", err)
	}

	// 4. 计算并返回 32 字节 SHA-256 摘要。
	return sha256.Sum256(encoded), nil
}
