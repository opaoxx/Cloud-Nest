package domain

// UserProfile 保存登录响应与当前用户接口所需的内部用户资料。
type UserProfile struct {
	// ID 是本次查询所属用户的主键。
	ID uint64
	// Username 是数据库中的登录名。
	Username string
	// DisplayName 是用户展示名称。
	DisplayName string
	// Status 是数据库账号状态，交给服务层判断。
	Status string
	// RootEntryID 是该用户真实的活动根目录主键。
	RootEntryID uint64
	// QuotaBytes 是逻辑存储配额的字节数。
	QuotaBytes uint64
	// UsedBytes 是已发布文件、回收站和保留版本占用的逻辑字节数。
	UsedBytes uint64
	// ReservedBytes 是未结束上传任务预占的字节数。
	ReservedBytes uint64
}
