package repository

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestNewPublishedFileEntryMappingIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `INSERT INTO stored_objects
		(id,owner_user_id,provider,bucket,object_key,file_size,verified_sha256,status,verified_at)
		VALUES(30,7,'MINIO','fixture-bucket','publish-empty',0,UNHEX(REPEAT('00',32)),'READY',UTC_TIMESTAMP(6))`); err != nil {
		t.Fatal(err)
	}

	for _, scenario := range []struct {
		name      string
		emptyFile bool
	}{
		{"ordinary entry", false},
		{"zero-byte entry without extension", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			record := publishedFileEntryMappingFixture()
			if scenario.emptyFile {
				record.Name, record.NameKey, record.Category, record.Extension = ".env", []byte(".env"), "OTHER", nil
				record.ObjectID, record.FileSize = 30, 0
			}
			// 1. 用隔离库验证 GORM 映射实际插入，内容时间取同一事务的 MySQL 时钟。
			err := store.withActiveUserTransaction(ctx, record.UserID, func(txContext context.Context, txStore *Store) error {
				if err := txStore.database.WithContext(txContext).Raw("SELECT UTC_TIMESTAMP(6)").Row().Scan(&record.ContentModifiedAt); err != nil {
					return err
				}
				result := txStore.database.WithContext(txContext).Create(&record)
				if result.Error != nil || result.RowsAffected != 1 {
					t.Fatal("发布文件映射未插入一行", result.Error)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}

			// 2. 通过活动名称键读回自增 ID、基本字段及默认版本和时间。
			entry, err := store.FindUserActiveFileEntryByName(ctx, record.UserID, record.ParentID, record.NameKey)
			if err != nil || entry == nil || entry.ID == 0 || entry.Name != record.Name || entry.FileSize != record.FileSize ||
				entry.EntryType != "FILE" || entry.Status != "ACTIVE" || entry.Revision != 1 || entry.CreatedAt.IsZero() ||
				!entry.CreatedAt.Equal(entry.ModifiedAt) || !entry.CreatedAt.Equal(entry.UpdatedAt) {
				t.Fatal("发布文件基础字段或默认值错误", entry, err)
			}
			display, err := store.FindUserFileEntryDisplay(ctx, record.UserID, entry.ID)
			if err != nil || display == nil || display.Category == nil || *display.Category != record.Category ||
				!reflect.DeepEqual(display.Extension, record.Extension) ||
				display.SourceDeviceID == nil || *display.SourceDeviceID != record.SourceDeviceID ||
				display.SourceDeviceName == nil || *display.SourceDeviceName != record.SourceDeviceName ||
				display.ContentModifiedAt == nil || !display.ContentModifiedAt.Equal(record.ContentModifiedAt) ||
				display.MIMEType != nil || display.ColorKey != nil {
				t.Fatal("发布文件展示字段、扩展名空值或内容时间错误", display, err)
			}
			var objectID uint64
			var activeNameKey []byte
			var rootOwnerID *uint64
			err = pool.QueryRowContext(ctx, "SELECT object_id,active_name_key,root_owner_id FROM file_entries WHERE user_id=? AND id=?", record.UserID, entry.ID).
				Scan(&objectID, &activeNameKey, &rootOwnerID)
			if err != nil || objectID != record.ObjectID || !bytes.Equal(activeNameKey, record.NameKey) || rootOwnerID != nil {
				t.Fatal("对象引用或生成列错误", err)
			}
		})
	}

	// 3. 检验活动名称唯一键和三类同用户引用外键。
	for _, scenario := range []struct {
		name   string
		parent uint64
		object uint64
		device uint64
		number uint16
	}{
		{"duplicate active name", 13, 29, 11, 1062},
		{"foreign parent", 14, 29, 11, 1452},
		{"foreign object", 13, 24, 11, 1452},
		{"foreign device", 13, 29, 12, 1452},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			record := publishedFileEntryMappingFixture()
			record.ParentID, record.ObjectID, record.SourceDeviceID = scenario.parent, scenario.object, scenario.device
			if scenario.number != 1062 {
				record.Name, record.NameKey = "guard.pdf", []byte("guard.pdf")
			}
			err := store.database.WithContext(ctx).Create(&record).Error
			var databaseError *mysql.MySQLError
			if !errors.As(err, &databaseError) || databaseError.Number != scenario.number {
				t.Fatal("未命中预期的数据库约束", err)
			}
		})
	}
}
