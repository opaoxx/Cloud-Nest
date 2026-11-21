package domain

import (
	"errors"
	"fmt"
)

const (
	uploadPartSizeUnitBytes uint64 = 1024 * 1024
	initialUploadPartSize   uint64 = 16 * uploadPartSizeUnitBytes
	maxUploadPartCount      uint64 = 10_000

	// DefaultMaxUploadSizeBytes 是 B4 新建上传任务的默认配置上限，50 GiB。
	DefaultMaxUploadSizeBytes uint64 = 50 * 1024 * uploadPartSizeUnitBytes
)

// ErrInvalidUploadPlanLimits 表示规划所需的配置或存储限制缺失。
var ErrInvalidUploadPlanLimits = errors.New("上传规划需要有效的文件、对象和单片大小上限")

// ErrInvalidUploadPartNumber 表示分片编号超出已保存计划的范围。
var ErrInvalidUploadPartNumber = errors.New("上传分片编号超出任务范围")

// UploadSizeLimitError 表示声明大小超限，并携带实际可接收的字节上限。
type UploadSizeLimitError struct {
	MaxSizeBytes uint64
}

func (limitError *UploadSizeLimitError) Error() string {
	return fmt.Sprintf("文件大小超过上传上限 %d 字节", limitError.MaxSizeBytes)
}

// UploadPlanLimits 保存新建上传的配置上限及已确认的存储能力，单位均为字节。
type UploadPlanLimits struct {
	MaxFileSizeBytes   uint64
	MaxObjectSizeBytes uint64
	MaxPartSizeBytes   uint64
}

// UploadPlan 保存上传模式及固定分片计划。SINGLE 的三个分片字段均为零。
type UploadPlan struct {
	Mode              string
	PartSizeBytes     uint64
	PartCount         uint32
	LastPartSizeBytes uint64
}

// PlanUpload 根据声明大小和限制计算上传计划。超限时返回实际可接收的最大值。
func PlanUpload(sizeBytes uint64, limits UploadPlanLimits) (UploadPlan, error) {
	// 1. 用配置、存储对象上限和分片容量计算实际文件上限。
	if limits.MaxFileSizeBytes == 0 || limits.MaxObjectSizeBytes == 0 || limits.MaxPartSizeBytes == 0 {
		return UploadPlan{}, ErrInvalidUploadPlanLimits
	}
	maxSize := min(limits.MaxFileSizeBytes, limits.MaxObjectSizeBytes)
	maxAlignedPartSize := limits.MaxPartSizeBytes / uploadPartSizeUnitBytes * uploadPartSizeUnitBytes
	if maxAlignedPartSize < initialUploadPartSize {
		maxSize = min(maxSize, MaxSingleUploadSizeBytes)
	} else if maxAlignedPartSize < ceilUploadDivision(maxSize, maxUploadPartCount) {
		// 分片容量小于已知的文件上限时才相乘，乘积不会溢出。
		maxSize = maxAlignedPartSize * maxUploadPartCount
	}
	if sizeBytes > maxSize {
		return UploadPlan{}, &UploadSizeLimitError{MaxSizeBytes: maxSize}
	}

	// 2. 0～16 MiB 使用 SINGLE，无需生成分片。
	if sizeBytes <= MaxSingleUploadSizeBytes {
		return UploadPlan{Mode: "SINGLE"}, nil
	}

	// 3. 将满足片数上限的最小片长按 MiB 向上取整，再计算片数和尾片。
	return planMultipartUpload(sizeBytes), nil
}

// ValidMultipartUploadLayout 核对已保存片长和片数与文件大小的关系，不读取当前存储限制。
func ValidMultipartUploadLayout(sizeBytes, partSizeBytes uint64, partCount uint32) bool {
	// 1. 检查文件大小分界和整 MiB 的片长约束。
	if sizeBytes <= MaxSingleUploadSizeBytes || partSizeBytes < initialUploadPartSize || partSizeBytes%uploadPartSizeUnitBytes != 0 {
		return false
	}
	// 2. 按保存的片长核对实际片数和协议上限。
	count := uint64(partCount)
	return count <= maxUploadPartCount && count == ceilUploadDivision(sizeBytes, partSizeBytes)
}

// ExpectedMultipartUploadPartSize 按已保存的分片计划计算指定编号的预期字节数。
// 计划异常返回 ErrInvalidUploadTaskData，编号越界返回 ErrInvalidUploadPartNumber。
func ExpectedMultipartUploadPartSize(sizeBytes, partSizeBytes uint64, partCount, partNumber uint32) (uint64, error) {
	// 1. 核对已保存的片长和片数，无需读取当前上传限制。
	if !ValidMultipartUploadLayout(sizeBytes, partSizeBytes, partCount) {
		return 0, ErrInvalidUploadTaskData
	}
	// 2. 拒绝范围以外的编号，再计算从零开始的字节偏移。
	if partNumber == 0 || partNumber > partCount {
		return 0, ErrInvalidUploadPartNumber
	}
	// 3. 通过核对的编号起点一定小于文件大小，乘法和减法均在范围内。
	offset := uint64(partNumber-1) * partSizeBytes
	return min(partSizeBytes, sizeBytes-offset), nil
}

// planMultipartUpload 计算超过 SINGLE 大小的声明所需的固定计划。
func planMultipartUpload(sizeBytes uint64) UploadPlan {
	requiredPartMiB := ceilUploadDivision(sizeBytes, maxUploadPartCount*uploadPartSizeUnitBytes)
	partSize := max(initialUploadPartSize, requiredPartMiB*uploadPartSizeUnitBytes)
	partCount := ceilUploadDivision(sizeBytes, partSize)
	return UploadPlan{
		Mode:              "MULTIPART",
		PartSizeBytes:     partSize,
		PartCount:         uint32(partCount),
		LastPartSizeBytes: sizeBytes - (partCount-1)*partSize,
	}
}

func ceilUploadDivision(value, divisor uint64) uint64 {
	quotient := value / divisor
	if value%divisor != 0 {
		quotient++
	}
	return quotient
}
