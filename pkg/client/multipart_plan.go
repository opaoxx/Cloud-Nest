package client

import "errors"

// MultipartPlan describes the stable part layout returned when the task is created.
type MultipartPlan struct {
	SizeBytes     int64
	PartSizeBytes int64
	PartCount     int
}

// MultipartPartRange is a zero-based file offset and a byte count.
type MultipartPartRange struct {
	OffsetBytes int64
	SizeBytes   int64
}

// PartRange validates the plan and locates a one-based part number in the file.
func (plan MultipartPlan) PartRange(partNumber int) (MultipartPartRange, error) {
	// 1. 按协议核对片长和片数。用减一再除避免向上取整时加法溢出。
	const mebibyte = 1024 * 1024
	const maxParts = 10_000
	if plan.SizeBytes <= MaxSingleUploadBytes || plan.SizeBytes > maxJSONSafeInteger ||
		plan.PartSizeBytes < MaxSingleUploadBytes || plan.PartSizeBytes > maxJSONSafeInteger || plan.PartSizeBytes%mebibyte != 0 ||
		plan.PartCount < 1 || plan.PartCount > maxParts || (plan.SizeBytes-1)/plan.PartSizeBytes+1 != int64(plan.PartCount) {
		return MultipartPartRange{}, errors.New("服务返回的分片计划与文件大小不一致")
	}
	if partNumber < 1 || partNumber > plan.PartCount {
		return MultipartPartRange{}, errors.New("分片编号超出任务范围")
	}

	// 2. 正常片按计划片长截取，最后一片只读取文件剩下的字节。
	offset := int64(partNumber-1) * plan.PartSizeBytes
	return MultipartPartRange{OffsetBytes: offset, SizeBytes: min(plan.PartSizeBytes, plan.SizeBytes-offset)}, nil
}
