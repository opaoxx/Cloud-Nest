package domain

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"
)

// CreateDirectoryOperation 是目录创建摘要与幂等记录共用的版本化操作名。
const CreateDirectoryOperation = "v1.directories.create"

// RenameEntryOperation 是目录项重命名摘要与幂等记录共用的版本化操作名。
const RenameEntryOperation = "v1.entries.rename"

// MoveEntryOperation 是目录项移动摘要与幂等记录共用的版本化操作名。
const MoveEntryOperation = "v1.entries.move"

// TrashEntryOperation 是目录项回收摘要与幂等记录共用的版本化操作名。
const TrashEntryOperation = "v1.entries.trash"

// RestoreEntryOperation 是单项恢复摘要与幂等记录共用的版本化操作名。
const RestoreEntryOperation = "v1.trash.restore"

// PurgeEntryOperation 是确认永久清理的摘要与幂等记录共用的版本化操作名。
const PurgeEntryOperation = "v1.trash.purge"

// CreateUploadOperation 是上传创建摘要与幂等记录共用的版本化操作名。
const CreateUploadOperation = "v1.uploads.create"

// CreateUploadReplacementOperation 是内容替换创建摘要与幂等记录共用的版本化操作名。
const CreateUploadReplacementOperation = "v1.entries.content-replacement.create"

// FileReuseOperation 是秒传请求摘要与幂等记录共用的版本化操作名。
const FileReuseOperation = "v1.files.reuse"

// ErrIdempotencyRecordNotFound 表示指定用户、操作和键的记录不存在。
var ErrIdempotencyRecordNotFound = errors.New("幂等记录不存在")

// ErrInvalidIdempotencyRecordData 表示幂等记录的范围、摘要或结果字段异常。
var ErrInvalidIdempotencyRecordData = errors.New("幂等记录数据异常")

// ErrIdempotencyRecordAlreadyExists 表示当前用户、操作和键已有记录。
var ErrIdempotencyRecordAlreadyExists = errors.New("幂等记录已存在")

// ErrIdempotencyRequestConflict 表示同一请求键关联了不同业务参数。
var ErrIdempotencyRequestConflict = errors.New("幂等键已用于不同请求参数")

// ErrIdempotencyRecordExpired 表示记录已到保留截止时间，需先核对已有资源状态。
var ErrIdempotencyRecordExpired = errors.New("幂等记录已到期，请先核对已有资源状态")

// IdempotencyResultInput 保存业务操作提供的请求范围及稳定结果字段。
type IdempotencyResultInput struct {
	// Operation 是服务端确定的版本化操作名。
	Operation string
	// IdempotencyKey 是已解析并保留大小写的客户端请求键。
	IdempotencyKey string
	// RequestHash 是规范化业务请求的固定摘要。
	RequestHash [sha256.Size]byte
	// ResourceType 指明结果资源类型。
	ResourceType string
	// ResourceID 为 nil 时表示结果未关联具体资源。
	ResourceID *uint64
	// ResponseStatus 是要保存的 HTTP 状态码。
	ResponseStatus int
	// ResponseBody 为 nil 时保存 SQL NULL，否则提供合法 UTF-8 JSON。
	ResponseBody json.RawMessage
	// ExpiresAt 由上层按保留策略计算，写入时转换为 UTC 微秒。
	ExpiresAt time.Time
}

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
