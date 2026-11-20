package domain

// CreatePublishedFileEntryInput 保存上传发布时创建普通文件目录项所需的字段。
// 所属用户取自发布事务，初始状态和发布时间由写入流程确定。
type CreatePublishedFileEntryInput struct {
	// ParentID 是上传任务当前的目标父目录 ID。
	ParentID uint64
	// Name 是已选定的发布展示名，保留大小写及 Unicode 写法。
	Name string
	// NameKey 是由发布名称生成的活动目录项判重键。
	NameKey []byte
	// Category 是按文件名派生的非空文件分类。
	Category string
	// Extension 是小写末级扩展名，无可用扩展名时为 nil。
	Extension *string
	// MIMEType 来自对对象字节的校验，无法识别时为 nil。
	MIMEType *string
	// ObjectID 是选定的 READY 对象 ID，可以复用已有对象。
	ObjectID uint64
	// SizeBytes 是已校验对象的字节数，保存为文件大小快照。
	SizeBytes uint64
	// SourceDeviceID 是上传任务的来源设备 ID。
	SourceDeviceID uint64
	// SourceDeviceName 是内容发布时读取的设备名称快照。
	SourceDeviceName string
}
