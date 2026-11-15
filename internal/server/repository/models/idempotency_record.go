package models

import "time"

// IdempotencyRecord 是幂等记录十一列的只读查询投影。
type IdempotencyRecord struct {
	// ID 对应幂等记录主键。
	ID uint64 `gorm:"column:id;->"`
	// UserID 对应记录所属用户。
	UserID uint64 `gorm:"column:user_id;->"`
	// Operation 对应版本化操作名。
	Operation string `gorm:"column:operation;->"`
	// IdempotencyKey 对应客户端请求键。
	IdempotencyKey string `gorm:"column:idempotency_key;->"`
	// RequestHash 承接 BINARY(32)，转换前核对实际字节数。
	RequestHash []byte `gorm:"column:request_hash;->"`
	// ResourceType 对应结果资源类型。
	ResourceType string `gorm:"column:resource_type;->"`
	// ResourceID 对应可空结果资源标识。
	ResourceID *uint64 `gorm:"column:resource_id;->"`
	// ResponseStatus 对应保存的 HTTP 状态码。
	ResponseStatus int `gorm:"column:response_status;->"`
	// ResponseBody 承接可空 JSON，保留 SQL NULL 与 JSON null。
	ResponseBody []byte `gorm:"column:response_body;->"`
	// ExpiresAt 对应保留截止时间。
	ExpiresAt time.Time `gorm:"column:expires_at;->"`
	// CreatedAt 对应记录创建时间。
	CreatedAt time.Time `gorm:"column:created_at;->"`
}

// TableName 指向已有幂等记录表。
func (IdempotencyRecord) TableName() string { return "idempotency_records" }
