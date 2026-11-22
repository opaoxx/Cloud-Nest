package service

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// FileReuseTransaction 汇集已认证账号事务内的秒传读取与写入能力。
type FileReuseTransaction interface {
	FileEntryPathReader
	FindUserVisibleReusableObject(ctx context.Context, userID uint64, digest [sha256.Size]byte, sizeBytes uint64) (*domain.StoredObject, error)
	CheckUserFileReuseQuota(ctx context.Context, sizeBytes uint64) error
	FindUserActiveFileEntryByName(ctx context.Context, userID, parentID uint64, nameKey []byte) (*domain.FileEntry, error)
	FindCurrentUserDevice(ctx context.Context) (*domain.UserDevice, error)
	CreateFileReuseEntry(ctx context.Context, candidate *domain.StoredObject, input domain.CreatePublishedFileEntryInput) (*domain.FileEntry, error)
	SettleUserFileReuseQuota(ctx context.Context, sizeBytes uint64) error
}

// FileReuseService 在同一账号事务中创建引用已有内容的新文件并结算空间。
type FileReuseService struct {
	// withTransaction 同步执行回调，传播失败，确认提交后返回 nil。
	withTransaction func(context.Context, domain.Actor, func(context.Context, FileReuseTransaction) error) error
}

// NewFileReuseService 绑定已认证账号事务入口。
func NewFileReuseService(withTransaction func(context.Context, domain.Actor, func(context.Context, FileReuseTransaction) error) error) *FileReuseService {
	if withTransaction == nil {
		panic("秒传服务需要已认证账号事务")
	}
	return &FileReuseService{withTransaction: withTransaction}
}

// Reuse 为已认证用户执行一次秒传写入，提交后返回新的文件目录项。
// sizeBytes 与 digest 表示调用方提交的完整内容大小与二进制摘要。
func (reuse *FileReuseService) Reuse(
	ctx context.Context, actor domain.Actor, parentID uint64, requestedName string, sizeBytes uint64, digest [sha256.Size]byte,
) (*domain.FileEntry, error) {
	// 1. 校验请求并进入已认证的账号事务。
	if ctx == nil || actor.UserID == 0 || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, fmt.Errorf("秒传需要上下文和完整认证身份")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := validateFileReuseDeclaration(parentID, requestedName, sizeBytes)
	if err != nil {
		return nil, err
	}
	var prepared *domain.FileEntry
	err = reuse.withTransaction(ctx, actor, func(txContext context.Context, txn FileReuseTransaction) error {
		if txContext == nil || txn == nil {
			return fmt.Errorf("秒传回调需要事务上下文和仓储能力")
		}

		// 2. 校验目标路径，按摘要和大小查找已有对象，预检配额。
		if _, err := ValidateFileReuseTarget(txContext, actor, parentID, txn); err != nil {
			return err
		}
		candidate, err := txn.FindUserVisibleReusableObject(txContext, actor.UserID, digest, sizeBytes)
		if err != nil {
			return err
		}
		if candidate == nil {
			return domain.ErrReusableContentNotFound
		}
		if candidate.FileSize != sizeBytes || candidate.VerifiedSHA256 == nil || *candidate.VerifiedSHA256 != digest {
			return domain.ErrInvalidStoredObjectData
		}
		if err := txn.CheckUserFileReuseQuota(txContext, sizeBytes); err != nil {
			return err
		}

		// 3. 选定文件名，读取当前设备，由仓储锁内复核对象并创建目录项。
		resolvedName, err := ResolveUploadPublicationName(txContext, actor.UserID, parentID, name, txn.FindUserActiveFileEntryByName)
		if err != nil {
			return err
		}
		device, err := txn.FindCurrentUserDevice(txContext)
		if err != nil {
			return err
		}
		input, err := BuildFileReuseEntryInput(actor, parentID, resolvedName, candidate, device)
		if err != nil {
			return err
		}
		entry, err := txn.CreateFileReuseEntry(txContext, candidate, *input)
		if err != nil {
			return err
		}
		if entry == nil || entry.ID == 0 || entry.UserID != actor.UserID || entry.ParentID == nil || *entry.ParentID != parentID ||
			entry.Name != resolvedName || entry.EntryType != "FILE" || entry.Status != "ACTIVE" || entry.FileSize != sizeBytes || entry.Revision != 1 {
			return domain.ErrInvalidFileEntryData
		}

		// 4. 按新文件的完整大小结算，将扣账或取消错误交给外层回滚。
		if err := txn.SettleUserFileReuseQuota(txContext, entry.FileSize); err != nil {
			return err
		}
		if err := txContext.Err(); err != nil {
			return err
		}
		entryCopy := *entry
		entryCopy.ParentID = &parentID
		prepared = &entryCopy
		return nil
	})

	// 5. 等待外层事务确认提交，返回已提交的目录项。
	if err != nil {
		return nil, err
	}
	if prepared == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	return prepared, nil
}
