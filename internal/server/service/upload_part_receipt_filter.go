package service

import "github.com/opaoxx/cloud-nest/internal/server/domain"

// FilterUploadPartReceipts 核对存储层已验证格式、按编号升序交付的完整回执。
// 返回的清单只包含符合任务固定片长的分片，缺片及片长不符的分片可由客户端补传。
func FilterUploadPartReceipts(fileSizeBytes, partSizeBytes uint64, partCount uint32, receipts []domain.UploadPartReceipt) ([]domain.UploadPartReceipt, error) {
	// 1. 先检查保存的计划，即使存储当前没有回执也要验证。
	if !domain.ValidMultipartUploadLayout(fileSizeBytes, partSizeBytes, partCount) {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if len(receipts) > int(partCount) {
		return nil, domain.ErrUploadStorageUnavailable
	}

	// 2. 核对编号范围和升序。已跳过的分片仍参与编号核对。
	available := make([]domain.UploadPartReceipt, 0, len(receipts))
	var previousNumber uint32
	for _, receipt := range receipts {
		if receipt.PartNumber <= previousNumber || receipt.PartNumber > partCount {
			return nil, domain.ErrUploadStorageUnavailable
		}
		previousNumber = receipt.PartNumber

		// 3. 用保存的计划计算精确片长，只交付长度匹配的分片。
		expectedSize, err := domain.ExpectedMultipartUploadPartSize(fileSizeBytes, partSizeBytes, partCount, receipt.PartNumber)
		if err != nil {
			return nil, err
		}
		if receipt.SizeBytes == expectedSize {
			available = append(available, receipt)
		}
	}
	return available, nil
}
