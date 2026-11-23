package domain

// MaxSyncChangePruneAccountPageSize 是一页最多检查的账号数。
const MaxSyncChangePruneAccountPageSize = 100

// SyncChangePruneCandidatePage 保存一页候选及按账号 ID 向后扫描的位置。
type SyncChangePruneCandidatePage struct {
	UserIDs         []uint64 // 首条保留日志已到期的账号。
	NextAfterUserID uint64   // 最后检查的账号 ID。没有检查到账号时等于输入游标。
	ScannedCount    int      // 本页检查的账号数。小于请求上限时已到扫描末尾。
}
