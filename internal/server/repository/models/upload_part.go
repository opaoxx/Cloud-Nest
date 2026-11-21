package models

import "time"

// UploadPart 映射已有 upload_parts 表中的单条完成快照。
type UploadPart struct {
	TaskID            uint64    `gorm:"column:task_id;primaryKey;autoIncrement:false"`
	PartNumber        uint32    `gorm:"column:part_number;primaryKey;autoIncrement:false"`
	ETag              string    `gorm:"column:etag"`
	SizeBytes         uint64    `gorm:"column:size"`
	ChecksumAlgorithm *string   `gorm:"column:checksum_algorithm"`
	ChecksumValue     *string   `gorm:"column:checksum_value"`
	ObservedAt        time.Time `gorm:"column:observed_at"`
}

// TableName 使用版本 2 迁移已建立的快照表。
func (UploadPart) TableName() string { return "upload_parts" }
