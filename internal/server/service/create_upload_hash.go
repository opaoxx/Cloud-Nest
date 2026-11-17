package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// BuildCreateUploadRequestHash 为上传创建声明生成版本化固定编码摘要。
// 名称沿用展示写法，文件哈希保留可空含义，失败返回零值摘要和领域错误。
func BuildCreateUploadRequestHash(input domain.CreateUploadInput) ([sha256.Size]byte, error) {
	// 1. 复用上传声明校验，取得规范化名称和独立摘要。
	prepared, err := ValidateCreateUploadInput(input)
	if err != nil {
		return [sha256.Size]byte{}, err
	}

	// 2. 将目录 ID、字节数和可空文件哈希转换为稳定文本。
	var expectedSHA256 *string
	if prepared.ExpectedSHA256 != nil {
		encodedHash := hex.EncodeToString(prepared.ExpectedSHA256[:])
		expectedSHA256 = &encodedHash
	}

	// 3. 按固定字段顺序编码版本化请求。
	request := struct {
		Operation string  `json:"operation"`
		ParentID  string  `json:"parent_id"`
		Name      string  `json:"name"`
		SizeBytes string  `json:"size_bytes"`
		SHA256    *string `json:"sha256"`
	}{
		Operation: domain.CreateUploadOperation,
		ParentID:  strconv.FormatUint(prepared.ParentID, 10),
		Name:      prepared.Name,
		SizeBytes: strconv.FormatUint(prepared.SizeBytes, 10),
		SHA256:    expectedSHA256,
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("编码上传创建请求摘要失败: %w", err)
	}

	// 4. 计算并返回 SHA-256 请求摘要。
	return sha256.Sum256(encoded), nil
}
