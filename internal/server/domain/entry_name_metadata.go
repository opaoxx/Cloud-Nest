package domain

// EntryNameMetadata 保存名称校验及名称派生的展示元数据。
type EntryNameMetadata struct {
	// Name 是去除首尾空白后保留原有大小写及 Unicode 写法的展示名称。
	Name string
	// Extension 是不带句点的小写后缀，文件夹、无后缀或后缀超出容量时为 nil。
	Extension *string
	// Category 是文件的 DOCUMENT/IMAGE/VIDEO/AUDIO/ARCHIVE/OTHER 分类，文件夹为 nil。
	Category *string
}
