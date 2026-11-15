package domain

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"
)

// CreateDirectoryOperation 是目录创建摘要与幂等记录共用的版本化操作名。
const CreateDirectoryOperation = "v1.directories.create"

// ErrIdempotencyRecordNotFound 表示指定用户、操作和键的记录不存在。
var ErrIdempotencyRecordNotFound = errors.New("幂等记录不存在")

// ErrInvalidIdempotencyRecordData 表示幂等记录的范围、摘要或结果字段异常。
var ErrInvalidIdempotencyRecordData = errors.New("幂等记录数据异常")

// IdempotencyRecord 保存幂等请求及已提交结果的数据库事实。
type IdempotencyRecord struct {
	// ID 是幂等记录的完整无符号主键。
	ID uint64
	// UserID 是已认证请求所属用户。
	UserID uint64
	// Operation 是服务端确定的版本化操作名。
	Operation string
	// IdempotencyKey 是已解析请求键，保留其大小写和原字符。
	IdempotencyKey string
	// RequestHash 是规范化业务参数的固定 32 字节摘要。
	RequestHash [sha256.Size]byte
	// ResourceType 指明结果资源的类型，供后续按操作校验。
	ResourceType string
	// ResourceID 为 nil 时表示记录未关联具体资源 ID。
	ResourceID *uint64
	// ResponseStatus 是已保存的 HTTP 状态码，范围为 100～599。
	ResponseStatus int
	// ResponseBody 保留 JSON 原文，nil 表示 SQL NULL，文本 null 表示 JSON null。
	ResponseBody json.RawMessage
	// ExpiresAt 是记录的保留截止时间，是否有效由后续服务判断。
	ExpiresAt time.Time
	// CreatedAt 是记录创建时间。
	CreatedAt time.Time
}
