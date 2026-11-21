package domain

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrUploadPartSessionMissing 表示 ListParts 报告存储会话不存在。应用层应先复查任务。
var ErrUploadPartSessionMissing = fmt.Errorf("分片上传会话不存在: %w", ErrUploadStorageUnavailable)

// ErrInvalidUploadPartReceiptListData 表示回执结果不满足公开响应的字段约束。
var ErrInvalidUploadPartReceiptListData = errors.New("分片回执结果异常")

var uploadPartETagPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// ValidUploadPartETag 判断去除外层引号后的分片 ETag 是否可作为公开回执。
func ValidUploadPartETag(value string) bool {
	return uploadPartETagPattern.MatchString(value)
}

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
