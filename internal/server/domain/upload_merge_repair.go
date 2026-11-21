package domain

// UploadMergeRepairEvidence 保存只读核对使用的任务快照、对象键及仍失配的分片编号。
// 任务包含原账号、会话与租约，写回时必须在账号事务逐项复核。
type UploadMergeRepairEvidence struct {
	Task        UploadTask
	ObjectKey   string
	PartNumbers []uint32
}
