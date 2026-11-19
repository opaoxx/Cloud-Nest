package service

// uploadRetryDelaySeconds 计算上传后台任务的退避秒数。
func uploadRetryDelaySeconds(attemptCount uint32) int {
	// 1. 先检查上限，较大的计数直接使用 30 秒。
	if attemptCount >= 5 {
		return 30
	}
	// 2. 从 1 秒开始，每多一次已记录的重试就翻倍。
	return 1 << attemptCount
}
