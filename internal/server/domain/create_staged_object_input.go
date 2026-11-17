package domain

// CreateStagedObjectInput 保存服务端创建暂存对象所需的位置和声明大小。
// 所属用户由可信事务提供，状态与初始校验事实由后续写入流程确定。
type CreateStagedObjectInput struct {
	// Provider 来自已校验的服务端存储配置，写入 provider。
	Provider string
	// Bucket 来自已校验的服务端存储配置，写入 bucket。
	Bucket string
	// ObjectKey 是服务端为本次暂存尝试生成的不可变对象键。
	ObjectKey string
	// SizeBytes 对应 file_size，保存完整无符号声明大小，零字节有效。
	SizeBytes uint64
}

// IsValid 检查必填存储文本的列长度及 ASCII 编码，保留全部输入原值。
// 存储配置、对象键生成、上传大小上限和配额分别由对应业务流程核对。
func (input CreateStagedObjectInput) IsValid() bool {
	// 1. 检查三项存储位置文本的必填及列长度。
	if len(input.Provider) == 0 || len(input.Provider) > 32 ||
		len(input.Bucket) == 0 || len(input.Bucket) > 63 ||
		len(input.ObjectKey) == 0 || len(input.ObjectKey) > 255 {
		return false
	}
	// 2. 按数据库 ASCII 字符集检查原始文本。
	for _, text := range []string{input.Provider, input.Bucket, input.ObjectKey} {
		for _, character := range text {
			if character > 127 {
				return false
			}
		}
	}
	// 3. 保留输入原值，返回有效结果。
	return true
}
