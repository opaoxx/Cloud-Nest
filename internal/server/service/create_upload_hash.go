package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// BuildCreateUploadRequestHash 为 SINGLE 声明生成既有版本的固定编码摘要。
// 名称沿用展示写法，文件哈希保留可空含义，失败返回零值摘要和领域错误。
func BuildCreateUploadRequestHash(input domain.CreateUploadInput) ([sha256.Size]byte, error) {
	// 1. 复用上传声明校验，取得规范化名称和独立摘要。
	prepared, err := ValidateCreateUploadInput(input)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return hashCreateUploadDeclaration(*prepared, "")
}

// BuildCreateUploadRequestHashWithMultipart 为两种上传模式计算可重放的请求摘要。
// 仅按请求声明计算。新建任务的配置和存储能力限制由规划入口校验。
func BuildCreateUploadRequestHashWithMultipart(input domain.CreateUploadInput) ([sha256.Size]byte, error) {
	// 1. 按大小选择模式，SINGLE 直接沿用已有摘要编码。
	if input.SizeBytes <= domain.MaxSingleUploadSizeBytes {
		return BuildCreateUploadRequestHash(input)
	}

	// 2. 校验并规范化 Multipart 声明，完整摘要是请求的一部分。
	name, err := validateCreateUploadTarget(input)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	if input.ExpectedSHA256 == nil {
		return [sha256.Size]byte{}, domain.ErrMultipartUploadSHA256Required
	}
	input.Name = name

	// 3. 按同一创建操作编码 Multipart 模式，生成稳定的请求摘要。
	return hashCreateUploadDeclaration(input, "MULTIPART")
}

func hashCreateUploadDeclaration(prepared domain.CreateUploadInput, uploadMode string) ([sha256.Size]byte, error) {
	// 1. 将目录 ID、字节数和可空文件哈希转换为稳定文本。
	var expectedSHA256 *string
	if prepared.ExpectedSHA256 != nil {
		encodedHash := hex.EncodeToString(prepared.ExpectedSHA256[:])
		expectedSHA256 = &encodedHash
	}

	// 2. 按固定字段顺序编码请求。SINGLE 省略模式字段，保留历史字节。
	request := struct {
		Operation  string  `json:"operation"`
		UploadMode string  `json:"upload_mode,omitempty"`
		ParentID   string  `json:"parent_id"`
		Name       string  `json:"name"`
		SizeBytes  string  `json:"size_bytes"`
		SHA256     *string `json:"sha256"`
	}{
		Operation:  domain.CreateUploadOperation,
		UploadMode: uploadMode,
		ParentID:   strconv.FormatUint(prepared.ParentID, 10),
		Name:       prepared.Name,
		SizeBytes:  strconv.FormatUint(prepared.SizeBytes, 10),
		SHA256:     expectedSHA256,
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("编码上传创建请求摘要失败: %w", err)
	}

	// 3. 计算并返回 SHA-256 请求摘要。
	return sha256.Sum256(encoded), nil
}
