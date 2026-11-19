package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// FindUserStoredObject 按可信用户归属读取物理对象，保留全部合法生命周期状态。
// 缺失返回 ErrStoredObjectNotFound，异常记录返回 ErrInvalidStoredObjectData。
// 返回值独立持有可空字段，具体操作的授权和写入由上层用户事务复核。
func (store *Store) FindUserStoredObject(ctx context.Context, userID, objectID uint64) (*domain.StoredObject, error) {
	// 1. 检查查询范围和上下文。
	if ctx == nil || userID == 0 || objectID == 0 {
		return nil, fmt.Errorf("查询存储对象需要上下文、非零用户 ID 和对象 ID")
	}

	// 2. 按主键和归属读取，再核对返回的对象 ID。
	query := store.database.WithContext(ctx).Where("owner_user_id = ? AND id = ?", userID, objectID)
	object, err := readStoredObject(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	if object.ID != objectID {
		return nil, domain.ErrInvalidStoredObjectData
	}
	return object, nil
}

// readStoredObject 共用完整对象的投影、校验和可空字段复制。
func readStoredObject(ctx context.Context, query *gorm.DB, userID uint64) (*domain.StoredObject, error) {
	// 1. 设置最多五秒期限，服从调用方更早的截止时间。
	queryContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询存储对象失败: %w", err)
	}

	// 2. 在调用方指定的范围内读取十七列。
	var record models.StoredObject
	queryError := query.WithContext(queryContext).
		Select("id", "owner_user_id", "provider", "bucket", "object_key", "storage_version_id",
			"file_size", "verified_sha256", "status", "dedup_hash", "storage_etag", "verified_at",
			"unreferenced_at", "gc_after", "last_error_code", "created_at", "updated_at").
		Take(&record).Error

	// 3. 处理取消、未找到及数据库故障。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询存储对象已取消: %w", err)
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrStoredObjectNotFound
	}
	if queryError != nil {
		return nil, fmt.Errorf("查询存储对象失败: %w", queryError)
	}

	// 4. 核对归属、存储位置、状态、摘要及回收约束。
	status := domain.StoredObjectStatus(record.Status)
	if record.ID == 0 || record.OwnerUserID != userID || !status.IsValid() ||
		record.Provider == "" || record.Bucket == "" || record.ObjectKey == "" {
		return nil, domain.ErrInvalidStoredObjectData
	}
	for _, text := range []*string{&record.Provider, &record.Bucket, &record.ObjectKey, record.StorageVersionID, record.StorageETag, record.LastErrorCode} {
		if text != nil {
			for _, character := range *text {
				if character > 127 {
					return nil, domain.ErrInvalidStoredObjectData
				}
			}
		}
	}
	for _, digest := range [][]byte{record.VerifiedSHA256, record.DedupHash} {
		if digest != nil && len(digest) != sha256.Size {
			return nil, domain.ErrInvalidStoredObjectData
		}
	}
	if status == domain.StoredObjectStatusReady {
		if record.VerifiedSHA256 == nil || record.VerifiedAt == nil || !bytes.Equal(record.DedupHash, record.VerifiedSHA256) {
			return nil, domain.ErrInvalidStoredObjectData
		}
	} else if record.DedupHash != nil {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if (record.UnreferencedAt == nil) != (record.GCAfter == nil) ||
		(record.UnreferencedAt != nil && record.GCAfter.Before(*record.UnreferencedAt)) {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 5. 检查时间并复制摘要及可空字段。
	for _, stamp := range []*time.Time{record.VerifiedAt, record.UnreferencedAt, record.GCAfter, &record.CreatedAt, &record.UpdatedAt} {
		if stamp != nil && (stamp.UTC().Year() < 1000 || stamp.UTC().Year() > 9999 || stamp.Nanosecond()%1000 != 0) {
			return nil, domain.ErrInvalidStoredObjectData
		}
	}
	var verifiedSHA256, dedupHash *[sha256.Size]byte
	if record.VerifiedSHA256 != nil {
		verifiedSHA256 = new([sha256.Size]byte)
		copy(verifiedSHA256[:], record.VerifiedSHA256)
	}
	if record.DedupHash != nil {
		dedupHash = new([sha256.Size]byte)
		copy(dedupHash[:], record.DedupHash)
	}
	result := &domain.StoredObject{
		ID:               record.ID,
		OwnerUserID:      record.OwnerUserID,
		Provider:         record.Provider,
		Bucket:           record.Bucket,
		ObjectKey:        record.ObjectKey,
		StorageVersionID: copyUploadTaskValue(record.StorageVersionID),
		FileSize:         record.FileSize,
		VerifiedSHA256:   verifiedSHA256,
		Status:           status,
		DedupHash:        dedupHash,
		StorageETag:      copyUploadTaskValue(record.StorageETag),
		VerifiedAt:       copyUploadTaskValue(record.VerifiedAt),
		UnreferencedAt:   copyUploadTaskValue(record.UnreferencedAt),
		GCAfter:          copyUploadTaskValue(record.GCAfter),
		LastErrorCode:    copyUploadTaskValue(record.LastErrorCode),
		CreatedAt:        record.CreatedAt.UTC(),
		UpdatedAt:        record.UpdatedAt.UTC(),
	}
	for _, stamp := range []*time.Time{result.VerifiedAt, result.UnreferencedAt, result.GCAfter} {
		if stamp != nil {
			*stamp = stamp.UTC()
		}
	}

	// 6. 复查取消，返回独立领域记录。
	if err := queryContext.Err(); err != nil {
		return nil, fmt.Errorf("查询存储对象已取消: %w", err)
	}
	return result, nil
}
