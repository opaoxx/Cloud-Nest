package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// maxUploadReplacementRequestSize 是内容替换 size_bytes 契约允许的最大 JSON 安全整数。
const maxUploadReplacementRequestSize uint64 = 1<<53 - 1

// BuildCreateUploadReplacementRequestHash 为规范化的替换声明生成固定摘要，失败返回零值。
// 文件可见性、实际版本和上传能力上限由首次创建流程检查。
func BuildCreateUploadReplacementRequestHash(
	entryID, expectedRevision, sizeBytes uint64, digest [sha256.Size]byte,
) ([sha256.Size]byte, error) {
	// 1. 核对目标 ID、预期版本和字节数的声明范围。
	if entryID == 0 || expectedRevision == 0 {
		return [sha256.Size]byte{}, fmt.Errorf("生成内容替换请求摘要需要非零目标 ID 和预期版本")
	}
	if sizeBytes > maxUploadReplacementRequestSize {
		return [sha256.Size]byte{}, fmt.Errorf("内容替换声明的文件大小超出 JSON 安全整数范围")
	}

	// 2. 按固定顺序编码操作名和四项参数，数值用十进制字符串保留精度。
	request := struct {
		Operation        string `json:"operation"`
		EntryID          string `json:"entry_id"`
		ExpectedRevision string `json:"expected_revision"`
		SizeBytes        string `json:"size_bytes"`
		SHA256           string `json:"sha256"`
	}{
		Operation:        domain.CreateUploadReplacementOperation,
		EntryID:          strconv.FormatUint(entryID, 10),
		ExpectedRevision: strconv.FormatUint(expectedRevision, 10),
		SizeBytes:        strconv.FormatUint(sizeBytes, 10),
		SHA256:           hex.EncodeToString(digest[:]),
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("编码内容替换请求摘要失败: %w", err)
	}

	// 3. 对编码后的请求计算 SHA-256。
	return sha256.Sum256(encoded), nil
}
