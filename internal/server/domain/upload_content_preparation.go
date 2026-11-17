package domain

// UploadContentPreparation 保存接收准备事务已确认的任务租约及当前暂存对象。
// 后续内容写入按 Task 中的租约版本与对象标识继续核对执行资格。
type UploadContentPreparation struct {
	// Task 包含本次接收使用的任务、租约持有者、期限及版本。
	Task *UploadTask
	// Object 包含当前接收尝试对应的不可变存储位置及声明大小。
	Object *StoredObject
}
