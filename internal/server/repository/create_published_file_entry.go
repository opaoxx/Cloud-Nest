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
	"gorm.io/gorm/clause"
)

// CreatePublishedFileEntry 在写回复核事务中为选定的 READY 对象创建文件目录项。
// 调用方先复核完整目标路径并准备写入输入，写入错误交给外层事务回滚。
func (store *Store) CreatePublishedFileEntry(ctx context.Context, task *domain.UploadTask, input domain.CreatePublishedFileEntryInput) (*domain.FileEntry, error) {
	// 1. 核对用户事务与任务对应的写入参数。
	if store == nil || store.database == nil || store.loginUserID == 0 || ctx == nil {
		return nil, fmt.Errorf("创建发布文件需要上下文和已锁定用户的事务")
	}
	if _, transactional := store.database.Statement.ConnPool.(gorm.TxCommitter); !transactional {
		return nil, fmt.Errorf("创建发布文件必须使用事务 Store")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	userID := store.loginUserID
	if task == nil || task.ID == 0 || task.UserID != userID || task.StagedObjectID == 0 ||
		task.LeaseVersion == 0 || task.LeaseOwner == nil || *task.LeaseOwner == "" || task.LeaseUntil == nil {
		return nil, domain.ErrInvalidUploadTaskData
	}
	record, err := newPublishedFileEntryRecord(userID, input)
	if err != nil {
		return nil, err
	}

	// 2. 复查当前任务、分片会话和租约，再核对父目录、对象和来源设备。
	if task.UploadMode != "SINGLE" && task.UploadMode != "MULTIPART" {
		return nil, domain.ErrUploadTaskNotReady
	}
	lockedStore := *store
	lockedStore.database = store.database.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"})
	current, err := lockedStore.findVerifyingUploadWithFullReservation(ctx, task, task.UploadMode)
	if err != nil {
		return nil, err
	}
	if current.TargetParentID != record.ParentID || current.DeviceID != record.SourceDeviceID || current.FileSize != record.FileSize {
		return nil, domain.ErrInvalidUploadTaskData
	}
	if err := store.validatePublishedFileParent(ctx, userID, record.ParentID); err != nil {
		return nil, err
	}
	object, err := lockedStore.FindUserStoredObject(ctx, userID, record.ObjectID)
	if err != nil {
		return nil, err
	}
	if object.Status != domain.StoredObjectStatusReady || object.FileSize != record.FileSize || object.VerifiedSHA256 == nil {
		return nil, domain.ErrInvalidStoredObjectData
	}
	if current.ExpectedSHA256 != nil && *object.VerifiedSHA256 != *current.ExpectedSHA256 {
		return nil, domain.ErrUploadChecksumMismatch
	}
	source, err := store.FindUploadSourceDevice(ctx, current)
	if err != nil {
		return nil, err
	}
	if source.Name != record.SourceDeviceName {
		return nil, domain.ErrInvalidDeviceData
	}

	// 3. 读取数据库时间，确认任务与租约仍有效。
	now, err := store.readPublishedFileEntryTime(ctx)
	if err != nil {
		return nil, err
	}
	if !now.Before(current.ExpiresAt) {
		return nil, domain.ErrUploadTaskNotReady
	}
	if !now.Before(*current.LeaseUntil) {
		return nil, domain.ErrUploadTaskBusy
	}
	record.ContentModifiedAt = now

	// 4. 写入并读回文件，依据实际创建时间再次确认任务和租约。
	entry, err := store.insertPublishedFileEntry(ctx, record)
	if err != nil {
		return nil, err
	}
	// 创建时间由 INSERT 使用数据库默认值产生，用它覆盖前置时间检查与写入间的间隔。
	if !entry.CreatedAt.Before(current.ExpiresAt) {
		return nil, domain.ErrUploadTaskNotReady
	}
	if !entry.CreatedAt.Before(*current.LeaseUntil) {
		return nil, domain.ErrUploadTaskBusy
	}
	return entry, nil
}

// newPublishedFileEntryRecord 将普通上传与秒传的写入输入映射为活动文件记录。
func newPublishedFileEntryRecord(userID uint64, input domain.CreatePublishedFileEntryInput) (*models.NewPublishedFileEntry, error) {
	// 1. 核对必需字段、名称键和可选的基础 MIME。
	if input.ParentID == 0 || input.ObjectID == 0 || input.SourceDeviceID == 0 || input.Category == "" {
		return nil, domain.ErrInvalidFileEntryData
	}
	if input.Name == "" || !utf8.ValidString(input.Name) || len(input.NameKey) == 0 || len(input.NameKey) > 768 || !utf8.Valid(input.NameKey) {
		return nil, domain.ErrInvalidEntryName
	}
	if input.MIMEType != nil && !domain.ValidDownloadMediaType(*input.MIMEType) {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 固定记录类型和初始状态，复制可变字段。
	return &models.NewPublishedFileEntry{
		UserID: userID, ParentID: input.ParentID, Name: input.Name, NameKey: bytes.Clone(input.NameKey),
		EntryType: "FILE", Status: "ACTIVE", Category: input.Category, Extension: copyUploadTaskValue(input.Extension),
		MIMEType: copyUploadTaskValue(input.MIMEType),
		ObjectID: input.ObjectID, FileSize: input.SizeBytes, SourceDeviceID: input.SourceDeviceID, SourceDeviceName: input.SourceDeviceName,
	}, nil
}

// validatePublishedFileParent 核对末级父目录，完整路径由调用服务在同一账号事务检查。
func (store *Store) validatePublishedFileParent(ctx context.Context, userID, parentID uint64) error {
	parent, err := store.FindUserFileEntry(ctx, userID, parentID)
	if err != nil {
		return err
	}
	if parent.Status != "ACTIVE" {
		return domain.ErrFileEntryNotFound
	}
	if parent.EntryType != "DIR" {
		return domain.ErrFileEntryNotDirectory
	}
	if parent.FileSize != 0 {
		return domain.ErrInvalidFileEntryData
	}
	return nil
}

// readPublishedFileEntryTime 读取数据库 UTC 微秒时间，供内容发布时间使用。
func (store *Store) readPublishedFileEntryTime(ctx context.Context) (time.Time, error) {
	var now time.Time
	queryError := store.database.WithContext(ctx).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&now)
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	if queryError != nil {
		return time.Time{}, fmt.Errorf("读取发布文件时间失败: %w", queryError)
	}
	now = now.UTC()
	if now.Year() < 1000 || now.Year() > 9999 || now.Nanosecond()%1000 != 0 {
		return time.Time{}, domain.ErrInvalidFileEntryData
	}
	return now, nil
}

// insertPublishedFileEntry 写入已复核的文件并读取数据库生成的初始字段。
func (store *Store) insertPublishedFileEntry(ctx context.Context, record *models.NewPublishedFileEntry) (*domain.FileEntry, error) {
	// 1. 写入文件目录项，处理活动同名冲突。
	result := store.database.WithContext(ctx).Create(record)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Error != nil {
		var databaseError *mysql.MySQLError
		if errors.As(result.Error, &databaseError) && databaseError.Number == 1062 {
			return nil, domain.ErrEntryNameConflict
		}
		return nil, fmt.Errorf("创建发布文件目录项失败: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, domain.ErrInvalidFileEntryData
	}

	// 2. 读回新文件，核对字段和实际插入时间。
	entry, err := store.FindUserActiveFileEntryByName(ctx, record.UserID, record.ParentID, record.NameKey)
	if errors.Is(err, domain.ErrFileEntryNotFound) {
		return nil, domain.ErrInvalidFileEntryData
	}
	if err != nil {
		return nil, err
	}
	if entry.Name != record.Name || entry.EntryType != record.EntryType || entry.FileSize != record.FileSize ||
		entry.Revision != 1 || entry.CreatedAt.Before(record.ContentModifiedAt) ||
		!entry.ModifiedAt.Equal(entry.CreatedAt) || !entry.UpdatedAt.Equal(entry.CreatedAt) {
		return nil, domain.ErrInvalidFileEntryData
	}
	return entry, nil
}
