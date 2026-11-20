package domain

// FileEntryContent 保存目录项指向的物理对象和已存媒体类型。
type FileEntryContent struct {
	// ID 是逻辑目录项的主键。
	ID uint64
	// UserID 是目录项所属用户。
	UserID uint64
	// ObjectID 是物理对象的主键，目录或已清理文件的空引用为 nil。
	ObjectID *uint64
	// MIMEType 是已保存的内容媒体类型，未保存时为 nil。
	MIMEType *string
}
