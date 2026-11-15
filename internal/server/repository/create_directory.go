package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// CreateUserDirectory 在 WithUserTransaction 回调中创建当前用户的普通文件夹。
// 展示名、名称键和颜色由上层校验，祖先可见性与深度由上层在同一用户事务中检查。
// 回调须向外返回本方法的错误，返回记录在外层事务提交成功后确认生效。
func (store *Store) CreateUserDirectory(ctx context.Context, parentID uint64, name string, nameKey []byte, colorKey *string) (*domain.FileEntry, error) {
	// 1. 检查已认证用户事务、输入和上下文。
	if ctx == nil || parentID == 0 || store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("创建文件夹需要上下文、非零父目录 ID 和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("创建文件夹必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	if name == "" || !utf8.ValidString(name) || len(nameKey) == 0 || len(nameKey) > 768 {
		return nil, domain.ErrInvalidEntryName
	}
	writeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := writeContext.Err(); err != nil {
		return nil, err
	}

	// 2. 按用户归属检查父目录及其自身状态。
	parent, err := store.FindUserFileEntry(writeContext, actor.UserID, parentID)
	if err != nil {
		return nil, err
	}
	if parent.Status != "ACTIVE" {
		return nil, domain.ErrFileEntryNotFound
	}
	if parent.EntryType != "DIR" {
		return nil, domain.ErrFileEntryNotDirectory
	}
	if parent.FileSize != 0 {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 3. 复制名称键和颜色，组装允许写入的目录字段。
	record := models.NewDirectory{
		UserID:    actor.UserID,
		ParentID:  parentID,
		Name:      name,
		NameKey:   bytes.Clone(nameKey),
		EntryType: "DIR",
		Status:    "ACTIVE",
	}
	if colorKey != nil {
		colorCopy := *colorKey
		record.ColorKey = &colorCopy
	}

	// 4. 插入目录，处理取消、名称冲突和数据库故障。
	result := store.database.WithContext(writeContext).
		Select("user_id", "parent_id", "name", "name_key", "entry_type", "status", "color_key").Create(&record)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		var databaseError *mysql.MySQLError
		if errors.As(result.Error, &databaseError) && databaseError.Number == 1062 {
			return nil, domain.ErrEntryNameConflict
		}
		return nil, fmt.Errorf("创建文件夹失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 5. 读回新目录的基础信息，核对初始状态后返回。
	entry, err := store.FindUserActiveFileEntryByName(writeContext, actor.UserID, parentID, record.NameKey)
	if errors.Is(err, domain.ErrFileEntryNotFound) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if err != nil {
		return nil, fmt.Errorf("读取新建文件夹失败: %w", err)
	}
	if entry.Name != name || entry.EntryType != "DIR" || entry.FileSize != 0 || entry.Revision != 1 {
		return nil, domain.ErrInvalidFileEntryData
	}
	return entry, nil
}
