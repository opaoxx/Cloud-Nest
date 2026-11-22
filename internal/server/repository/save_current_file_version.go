package repository

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SaveCurrentFileVersion 在所属账号事务中为将被替换的当前文件保存一条历史引用。
// 调用方须在同一事务更新当前引用并结算配额。失败须交给外层回滚。
func (store *Store) SaveCurrentFileVersion(ctx context.Context, entryID, expectedRevision uint64, retentionDays int) (*domain.FileVersion, error) {
	// 1. 要求持锁账号事务，按归属锁读文件、完整父链及预期版本。
	if ctx == nil || store == nil || store.database == nil || store.loginUserID == 0 ||
		entryID == 0 || expectedRevision == 0 || retentionDays <= 0 {
		return nil, fmt.Errorf("保存文件历史需要账号事务、有效文件版本和正的保留天数")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("保存文件历史必须使用账号事务 Store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	userID := store.loginUserID
	lockedEntryStore := *store
	lockedEntryStore.database = store.database.WithContext(ctx).
		Table("file_entries FORCE INDEX (uk_entries_owner_id)").
		Clauses(clause.Locking{Strength: "UPDATE"})
	entry, err := lockedEntryStore.FindUserFileEntry(ctx, userID, entryID)
	if err != nil {
		return nil, err
	}
	if entry.Status != "ACTIVE" || entry.EntryType != "FILE" || entry.ParentID == nil {
		return nil, domain.ErrFileEntryNotFound
	}
	if err := store.checkLockedActiveEntryAncestors(ctx, userID, *entry.ParentID); err != nil {
		return nil, err
	}
	if entry.Revision != expectedRevision {
		return nil, &domain.EntryRevisionConflictError{CurrentRevision: entry.Revision}
	}
	if entry.Revision == math.MaxUint64 {
		return nil, domain.ErrEntryRevisionExhausted
	}

	// 2. 按当前引用锁读旧对象，核对已校验状态、逻辑大小和回收计时。
	content, err := lockedEntryStore.FindUserFileEntryContent(ctx, userID, entryID)
	if err != nil {
		return nil, err
	}
	if content.ObjectID == nil {
		return nil, domain.ErrInvalidFileEntryData
	}
	lockedObjectStore := *store
	lockedObjectStore.database = store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"})
	object, err := lockedObjectStore.FindUserStoredObject(ctx, userID, *content.ObjectID)
	if err != nil {
		return nil, err
	}
	if object.Status != domain.StoredObjectStatusReady || object.VerifiedSHA256 == nil || object.VerifiedAt == nil ||
		object.FileSize != entry.FileSize || object.UnreferencedAt != nil || object.GCAfter != nil {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 3. 按数据库 UTC 时间写入单条历史。重复键保留已存在的引用与截止时间。
	createdAt, err := store.readPublishedFileEntryTime(ctx)
	if err != nil {
		return nil, err
	}
	// MySQL 的日期上界为 9999 年，先限制输入以避免日历运算溢出。
	if retentionDays > (10000-createdAt.Year())*366 {
		return nil, domain.ErrInvalidFileVersionData
	}
	retainedUntil := createdAt.AddDate(0, 0, retentionDays)
	if retainedUntil.Year() > 9999 {
		return nil, domain.ErrInvalidFileVersionData
	}
	transaction := store.database.WithContext(ctx)
	inserted := transaction.Exec(`INSERT INTO file_versions
		(user_id, entry_id, revision, object_id, file_size, retained_until, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, userID, entry.ID, entry.Revision, object.ID, entry.FileSize, retainedUntil, createdAt)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if inserted.Error != nil {
		var databaseError *mysql.MySQLError
		if errors.As(inserted.Error, &databaseError) && databaseError.Number == 1062 {
			return nil, domain.ErrFileVersionConflict
		}
		return nil, fmt.Errorf("写入文件历史版本失败: %w", inserted.Error)
	}
	if inserted.RowsAffected != 1 {
		return nil, domain.ErrInvalidFileVersionData
	}

	// 4. 读回并核对已写入的事实，交由外层确认提交。
	var saved domain.FileVersion
	err = transaction.Raw(`SELECT id,user_id,entry_id,revision,object_id,file_size,created_at,retained_until
		FROM file_versions WHERE user_id=? AND entry_id=? AND revision=?`, userID, entry.ID, entry.Revision).Row().
		Scan(&saved.ID, &saved.UserID, &saved.EntryID, &saved.Revision, &saved.ObjectID, &saved.FileSize, &saved.CreatedAt, &saved.RetainedUntil)
	if err != nil {
		return nil, fmt.Errorf("读回文件历史版本失败: %w", err)
	}
	if saved.ID == 0 || saved.UserID != userID || saved.EntryID != entry.ID || saved.Revision != entry.Revision ||
		saved.ObjectID != object.ID || saved.FileSize != entry.FileSize || !saved.CreatedAt.Equal(createdAt) ||
		!saved.RetainedUntil.Equal(retainedUntil) {
		return nil, domain.ErrInvalidFileVersionData
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &saved, nil
}
