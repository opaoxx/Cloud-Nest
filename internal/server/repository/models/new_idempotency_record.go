package models

import "time"

// NewIdempotencyRecord 映射保存结果所需的九列，标识和创建时间由数据库生成。
type NewIdempotencyRecord struct {
	// UserID 来自已认证用户事务。
	UserID uint64 `gorm:"column:user_id;<-:create"`
	// Operation 对应服务端版本化操作名。
	Operation string `gorm:"column:operation;<-:create"`
	// IdempotencyKey 对应已解析的请求键。
	IdempotencyKey string `gorm:"column:idempotency_key;<-:create"`
	// RequestHash 保存固定 32 字节请求摘要。
	RequestHash []byte `gorm:"column:request_hash;<-:create"`
	// ResourceType 对应结果资源类型。
	ResourceType string `gorm:"column:resource_type;<-:create"`
	// ResourceID 对应可空结果标识。
	ResourceID *uint64 `gorm:"column:resource_id;<-:create"`
	// ResponseStatus 对应原 HTTP 状态码。
	ResponseStatus int `gorm:"column:response_status;<-:create"`
	// ResponseBody 使用可空文本传递 JSON，保留 SQL NULL 与 JSON null。
	ResponseBody *string `gorm:"column:response_body;<-:create"`
	// ExpiresAt 对应 UTC 微秒保留截止时间。
	ExpiresAt time.Time `gorm:"column:expires_at;<-:create"`
}

// TableName 指向已有的幂等记录表。
func (NewIdempotencyRecord) TableName() string { return "idempotency_records" }
