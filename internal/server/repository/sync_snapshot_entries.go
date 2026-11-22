package repository

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// ListSyncSnapshotEntries 读取当前同步快照内可达的活动后代及已验证文件摘要。
// 仅在 WithUserSyncSnapshot 回调中使用。按数值 ID 升序返回，空树返回空切片。
func (store *Store) ListSyncSnapshotEntries(ctx context.Context) ([]domain.SyncSnapshotEntry, error) {
	// 1. 取得当前快照已经核对的用户和根目录，沿用事务的五秒读取上限。
	if ctx == nil || store.syncSnapshot == nil {
		return nil, fmt.Errorf("读取同步快照条目需要上下文和已绑定的同步快照")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope := store.syncSnapshot

	// 2. 沿同一用户的活动目录向下遍历，并在同一视图中关联符合要求的对象。
	// 多取一层和一项来识别超限。递归按根目录、允许条目及额外探测项合计限量。
	// LEFT JOIN 保留未关联到有效对象的文件，后续整次读取报错，避免遗漏文件。
	const query = `WITH RECURSIVE active_entries AS (
		SELECT id, user_id, entry_type, CAST(0 AS UNSIGNED) AS depth
		FROM file_entries
		WHERE user_id = ? AND id = ? AND parent_id IS NULL
		  AND status = 'ACTIVE' AND entry_type = 'DIR'
		UNION ALL
		SELECT child.id, child.user_id, child.entry_type, parent.depth + 1
		FROM active_entries AS parent
		JOIN file_entries AS child ON child.user_id = parent.user_id AND child.parent_id = parent.id
		WHERE parent.entry_type = 'DIR' AND child.status = 'ACTIVE' AND parent.depth < ?
		LIMIT ?
	)
	SELECT entry.id, entry.parent_id, entry.name, entry.entry_type, entry.revision,
	       entry.file_size, active.depth, object.verified_sha256, object.verified_at
	FROM active_entries AS active
	JOIN file_entries AS entry ON entry.user_id = active.user_id AND entry.id = active.id
	LEFT JOIN stored_objects AS object
	  ON entry.entry_type = 'FILE' AND object.owner_user_id = entry.user_id AND object.id = entry.object_id
	 AND object.status = 'READY' AND object.file_size = entry.file_size
	WHERE active.depth > 0
	ORDER BY entry.id ASC`
	var rows []struct {
		ID             uint64
		ParentID       uint64
		Name           string
		EntryType      string
		Revision       uint64
		FileSize       uint64
		Depth          uint64
		VerifiedSHA256 []byte `gorm:"column:verified_sha256"`
		VerifiedAt     *time.Time
	}
	result := store.database.WithContext(ctx).
		Raw(query, scope.userID, scope.rootEntryID, domain.MaxFileEntryDepth+1, domain.MaxSyncSnapshotEntries+2).Scan(&rows)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, fmt.Errorf("读取同步快照条目失败: %w", result.Error)
	}

	// 3. 核对目录结构与内容校验事实。超限或可见文件数据异常时整体失败。
	if len(rows) > domain.MaxSyncSnapshotEntries {
		return nil, domain.ErrSyncSnapshotTooLarge
	}
	entries := make([]domain.SyncSnapshotEntry, 0, len(rows))
	for _, row := range rows {
		if row.Depth > domain.MaxFileEntryDepth || row.ID == 0 || row.ParentID == 0 || row.Revision == 0 {
			return nil, domain.ErrInvalidFileEntryData
		}
		entry := domain.SyncSnapshotEntry{
			ID: row.ID, ParentID: row.ParentID, Name: row.Name, EntryType: row.EntryType,
			Revision: row.Revision, FileSize: row.FileSize,
		}
		switch row.EntryType {
		case "DIR":
			if row.FileSize != 0 {
				return nil, domain.ErrInvalidFileEntryData
			}
		case "FILE":
			if len(row.VerifiedSHA256) != sha256.Size || row.VerifiedAt == nil || row.VerifiedAt.Year() < 1000 {
				return nil, domain.ErrInvalidStoredObjectData
			}
			digest := [sha256.Size]byte(row.VerifiedSHA256)
			entry.VerifiedSHA256 = &digest
		default:
			return nil, domain.ErrInvalidFileEntryData
		}
		entries = append(entries, entry)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}
