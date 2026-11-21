package domain

import "fmt"

// ErrUploadPartSessionMissing 表示 ListParts 报告存储会话不存在。应用层应先复查任务。
var ErrUploadPartSessionMissing = fmt.Errorf("分片上传会话不存在: %w", ErrUploadStorageUnavailable)

// UploadPartReceipt 保存存储端返回的编号、实际字节数和分片回执。
type UploadPartReceipt struct {
	PartNumber     uint32
	SizeBytes      uint64
	ETag           string
	ChecksumSHA256 string
}

// UploadPartReceiptPage 是一次 ListParts 读取。NextMarker 为零表示已到末页。
type UploadPartReceiptPage struct {
	Parts      []UploadPartReceipt
	NextMarker uint32
}

// UploadPartReceiptList 保存任务的固定分片计划和本轮核对后的可用回执。
type UploadPartReceiptList struct {
	PartSizeBytes uint64
	PartCount     uint32
	Parts         []UploadPartReceipt
}
