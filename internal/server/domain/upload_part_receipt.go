package domain

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
