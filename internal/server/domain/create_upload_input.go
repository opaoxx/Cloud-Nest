package domain

import (
	"crypto/sha256"
	"errors"
	"unicode/utf8"
)

// MaxSingleUploadSizeBytes 是首期 SINGLE 上传允许声明的最大字节数，16 MiB。
const MaxSingleUploadSizeBytes uint64 = 16 * 1024 * 1024

// ErrInvalidUploadParentID 表示上传声明缺少非零目标目录标识。
var ErrInvalidUploadParentID = errors.New("上传目标目录 ID 须为非零值")

// ErrUploadFileTooLarge 表示文件声明超过首期 SINGLE 上传能力上限。
var ErrUploadFileTooLarge = errors.New("文件大小不能超过 16 MiB")

// CreateUploadInput 保存创建单文件上传任务所需的业务声明。
// 用户和来源设备由可信 Actor 提供，完整命名规则由服务复用统一校验。
type CreateUploadInput struct {
	// ParentID 对应 upload_tasks.target_parent_id，保留完整无符号目录 ID。
	ParentID uint64
	// Name 保留原始非空 UTF-8 名称，供服务校验后写入 requested_name。
	Name string
	// SizeBytes 对应 file_size，零字节文件使用相同上传流程。
	SizeBytes uint64
	// ExpectedSHA256 为 nil 时表示未声明哈希，非空时保存完整 32 字节预期摘要。
	ExpectedSHA256 *[sha256.Size]byte
}

// IsValid 检查创建输入的基本声明，保留原始名称和预期哈希。
// 目标归属、路径、完整命名、配额及文件内容在后续服务流程中核对。
func (input CreateUploadInput) IsValid() bool {
	// 1. 检查目标目录 ID 和原始名称编码。
	if input.ParentID == 0 || input.Name == "" || !utf8.ValidString(input.Name) {
		return false
	}
	// 2. 按首期 SINGLE 上传能力检查声明大小。
	return input.SizeBytes <= MaxSingleUploadSizeBytes
}
