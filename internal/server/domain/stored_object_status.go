package domain

// StoredObjectStatus 保存 stored_objects.status 的精确状态值。
type StoredObjectStatus string

const (
	// StoredObjectStatusStaging 表示已建立暂存位置，等待或正在写入内容。
	StoredObjectStatusStaging StoredObjectStatus = "STAGING"
	// StoredObjectStatusVerifying 表示等待或正在验证对象内容。
	StoredObjectStatusVerifying StoredObjectStatus = "VERIFYING"
	// StoredObjectStatusReady 表示内容已验证，可供发布及新引用使用。
	StoredObjectStatusReady StoredObjectStatus = "READY"
	// StoredObjectStatusOrphaned 表示上传结束或暂存副本被替换后等待回收。
	StoredObjectStatusOrphaned StoredObjectStatus = "ORPHANED"
	// StoredObjectStatusDeleting 表示已记录删除意图，等待物理删除完成。
	StoredObjectStatusDeleting StoredObjectStatus = "DELETING"
	// StoredObjectStatusDeleted 表示物理内容已删除或已确认不存在。
	StoredObjectStatusDeleted StoredObjectStatus = "DELETED"
)

// IsValid 检查状态是否属于数据库允许的对象生命周期阶段。
func (status StoredObjectStatus) IsValid() bool {
	// 1. 按数据库约束精确匹配六种合法对象状态。
	switch status {
	case StoredObjectStatusStaging, StoredObjectStatusVerifying, StoredObjectStatusReady,
		StoredObjectStatusOrphaned, StoredObjectStatusDeleting, StoredObjectStatusDeleted:
		return true
	}
	// 2. 其余值返回 false。
	return false
}
