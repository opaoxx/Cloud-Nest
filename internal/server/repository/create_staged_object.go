package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"gorm.io/gorm"
)

// CreateUserStagedObject 在 WithUserTransaction 回调中创建当前用户的暂存对象。
// 存储配置、对象键生成及上传大小能力由上层核对，重复位置返回固定冲突错误。
// 回调须向外返回本方法的错误，返回记录在外层事务提交成功后确认生效。
func (store *Store) CreateUserStagedObject(ctx context.Context, input domain.CreateStagedObjectInput) (*domain.StoredObject, error) {
	// 1. 检查已认证用户事务和上下文。
	if ctx == nil || store.authenticatedActor == nil || store.loginUserID == 0 {
		return nil, fmt.Errorf("创建暂存对象需要上下文和已认证用户事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("创建暂存对象必须使用已认证事务 Store")
	}
	actor := *store.authenticatedActor
	if actor.UserID != store.loginUserID || actor.DeviceID == 0 || actor.SessionID == 0 {
		return nil, domain.ErrInvalidSessionData
	}
	writeContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := writeContext.Err(); err != nil {
		return nil, err
	}

	// 2. 校验创建参数，组装六列 STAGING 记录。
	if !input.IsValid() {
		return nil, domain.ErrInvalidStoredObjectData
	}
	record := models.NewStagedObject{
		OwnerUserID: actor.UserID,
		Provider:    input.Provider,
		Bucket:      input.Bucket,
		ObjectKey:   input.ObjectKey,
		FileSize:    input.SizeBytes,
		Status:      string(domain.StoredObjectStatusStaging),
	}

	// 3. 插入对象，处理取消、位置冲突和数据库故障。
	result := store.database.WithContext(writeContext).
		Select("owner_user_id", "provider", "bucket", "object_key", "file_size", "status").Create(&record)
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		var databaseError *mysql.MySQLError
		if errors.As(result.Error, &databaseError) && databaseError.Number == 1062 {
			return nil, domain.ErrStoredObjectLocationConflict
		}
		return nil, fmt.Errorf("创建暂存对象失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 4. 按用户归属及位置唯一键取得对象标识。
	var identity models.StoredObject
	queryError := store.database.WithContext(writeContext).
		Select("id", "owner_user_id").
		Where("owner_user_id = ? AND provider = ? AND bucket = ? AND object_key = ?", actor.UserID, input.Provider, input.Bucket, input.ObjectKey).
		Take(&identity).Error
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	if errors.Is(queryError, gorm.ErrRecordNotFound) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if queryError != nil {
		return nil, fmt.Errorf("定位新建暂存对象失败: %w", queryError)
	}
	if identity.ID == 0 || identity.OwnerUserID != actor.UserID {
		return nil, domain.ErrInvalidStoredObjectData
	}

	// 5. 复用完整读取，核对新建事实后交由外层事务确认。
	created, err := store.FindUserStoredObject(writeContext, actor.UserID, identity.ID)
	if errors.Is(err, domain.ErrStoredObjectNotFound) {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if err != nil {
		return nil, fmt.Errorf("读取新建暂存对象失败: %w", err)
	}
	if created.Provider != input.Provider || created.Bucket != input.Bucket || created.ObjectKey != input.ObjectKey ||
		created.FileSize != input.SizeBytes || created.Status != domain.StoredObjectStatusStaging ||
		created.StorageVersionID != nil || created.VerifiedSHA256 != nil || created.DedupHash != nil ||
		created.StorageETag != nil || created.VerifiedAt != nil || created.UnreferencedAt != nil ||
		created.GCAfter != nil || created.LastErrorCode != nil {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if err := writeContext.Err(); err != nil {
		return nil, err
	}
	return created, nil
}
