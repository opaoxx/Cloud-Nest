package repository

import (
	"context"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// CreateFileReuseEntry 在认证账号事务中为同账号已有对象创建活动文件。
// 调用方先校验完整目标路径和名称，并在同一事务内处理配额、幂等结果和错误回滚。
func (store *Store) CreateFileReuseEntry(
	ctx context.Context, candidate *domain.StoredObject, input domain.CreatePublishedFileEntryInput,
) (*domain.FileEntry, error) {
	// 1. 从认证事务读取当前设备，核对写入参数。
	source, err := store.FindCurrentUserDevice(ctx)
	if err != nil {
		return nil, err
	}
	record, err := newPublishedFileEntryRecord(source.UserID, input)
	if err != nil {
		return nil, err
	}
	if record.SourceDeviceID != source.ID || record.SourceDeviceName != source.Name {
		return nil, domain.ErrInvalidDeviceData
	}

	// 2. 复核目标父目录和候选对象的可见引用。
	if err := store.validatePublishedFileParent(ctx, source.UserID, record.ParentID); err != nil {
		return nil, err
	}
	if candidate == nil {
		return nil, domain.ErrReusableContentNotFound
	}
	if candidate.ID != record.ObjectID || candidate.FileSize != record.FileSize {
		return nil, domain.ErrInvalidStoredObjectData
	}
	locked, err := store.RecheckVisibleReusableObject(ctx, candidate)
	if err != nil {
		return nil, err
	}
	if locked == nil {
		return nil, domain.ErrReusableContentNotFound
	}

	// 3. 读取数据库时间，准备写入新文件目录项。
	record.ContentModifiedAt, err = store.readPublishedFileEntryTime(ctx)
	if err != nil {
		return nil, err
	}

	// 4. 写入并读回新目录项，由外层事务决定提交或回滚。
	return store.insertPublishedFileEntry(ctx, record)
}
