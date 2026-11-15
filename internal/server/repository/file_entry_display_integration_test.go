package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestFindUserFileEntryDisplayIntegration(t *testing.T) {
	// 1. 复用正式表独占库，建立真实来源设备、文件、目录及历史记录。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','展示用户','fixture_hash','ACTIVE'),(2,'fixture_two','禁用用户','fixture_hash','DISABLED'),(3,'fixture_three','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','设备当前名称','WEB')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-entry-display',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,color_key) VALUES(11,1,NULL,'',X'','DIR',NULL),(22,2,NULL,'',X'','DIR',NULL),(33,3,NULL,'',X'','DIR',NULL),(12,1,11,'彩色目录',CAST('彩色目录' AS BINARY),'DIR','green')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,mime_type,object_id,file_size,source_device_id,source_device_name,content_modified_at) VALUES(13,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT','md','text/markdown',41,2048,13,'  发布时设备 🎮  ','2020-01-02 03:04:05.123456')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,color_key,status,deleted_at) VALUES(14,1,11,'回收目录',CAST('回收目录' AS BINARY),'DIR','sand','TRASHED','2020-01-02 03:04:05.123456')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,color_key,status,purge_started_at) VALUES(15,1,11,'清理目录',CAST('清理目录' AS BINARY),'DIR','green','PURGING','2020-01-02 03:04:05.123456')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,file_size,source_device_id,source_device_name,status,purged_at) VALUES(16,1,11,'已清理.md',CAST('已清理.md' AS BINARY),'FILE','DOCUMENT','md',2048,13,'  发布时设备 🎮  ','PURGED','2020-01-02 03:04:05.123456')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,object_id,file_size,source_device_name) VALUES(17,1,14,'旧文件',CAST('旧文件' AS BINARY),'FILE','OTHER','',41,2048,'旧发布设备')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,source_device_id) VALUES(18,1,12,'缺少来源名',CAST('缺少来源名' AS BINARY),'FILE','OTHER',41,2048,13)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立展示字段夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	category, extension, mimeType := "DOCUMENT", "md", "text/markdown"
	sourceID := uint64(13)
	sourceName, green, sand := "  发布时设备 🎮  ", "green", "sand"
	stamp := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
	file := domain.FileEntryDisplay{
		ID: 13, UserID: 1, Category: &category, Extension: &extension, MIMEType: &mimeType,
		SourceDeviceID: &sourceID, SourceDeviceName: &sourceName, ContentModifiedAt: &stamp,
	}
	check := func(t *testing.T, queryContext context.Context, reader *Store, expected domain.FileEntryDisplay) {
		t.Helper()
		actual, err := reader.FindUserFileEntryDisplay(queryContext, expected.UserID, expected.ID)
		if err != nil || !reflect.DeepEqual(actual, &expected) {
			t.Fatal("展示字段、NULL 或原文读回错误", err)
		}
	}

	// 2. 核对展示原文、空值及生命周期事实，后续服务使用已有路径检查。
	t.Run("file directory root and historical fields", func(t *testing.T) {
		check(t, ctx, store, file)
		for _, expected := range []domain.FileEntryDisplay{
			{ID: 11, UserID: 1},
			{ID: 12, UserID: 1, ColorKey: &green},
			{ID: 14, UserID: 1, ColorKey: &sand},
			{ID: 15, UserID: 1, ColorKey: &green},
			{ID: 16, UserID: 1, Category: &category, Extension: &extension, SourceDeviceID: &sourceID, SourceDeviceName: &sourceName},
			{ID: 22, UserID: 2},
			{ID: 33, UserID: 3},
		} {
			check(t, ctx, store, expected)
		}
		other, empty, oldSource := "OTHER", "", "旧发布设备"
		check(t, ctx, store, domain.FileEntryDisplay{ID: 17, UserID: 1, Category: &other, Extension: &empty, SourceDeviceName: &oldSource})
		check(t, ctx, store, domain.FileEntryDisplay{ID: 18, UserID: 1, Category: &other, SourceDeviceID: &sourceID})
	})
	t.Run("missing and foreign targets", func(t *testing.T) {
		for _, pair := range [][2]uint64{{1, 33}, {3, 13}, {1, 999}, {99, 13}} {
			result, err := store.FindUserFileEntryDisplay(ctx, pair[0], pair[1])
			if result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
				t.Fatal("展示字段归属隔离或缺失结果错误", err)
			}
		}
	})
	t.Run("independent nullable results and read only facts", func(t *testing.T) {
		before, err := store.FindUserFileEntry(ctx, 1, 13)
		if err != nil {
			t.Fatal(err)
		}
		first, err := store.FindUserFileEntryDisplay(ctx, 1, 13)
		if err != nil || first == nil {
			t.Fatal(err)
		}
		second, err := store.FindUserFileEntryDisplay(ctx, 1, 13)
		if err != nil || !reflect.DeepEqual(second, &file) {
			t.Fatal(err)
		}
		*first.Category, *first.Extension, *first.MIMEType = "OTHER", "changed", "changed"
		*first.SourceDeviceID, *first.SourceDeviceName, *first.ContentModifiedAt = 999, "本地修改", time.Time{}
		if !reflect.DeepEqual(second, &file) {
			t.Fatal("修改一次结果影响了另一查询")
		}
		check(t, ctx, store, file)
		colored, err := store.FindUserFileEntryDisplay(ctx, 1, 12)
		if err != nil || colored == nil || colored.ColorKey == nil {
			t.Fatal(err)
		}
		*colored.ColorKey = "local"
		check(t, ctx, store, domain.FileEntryDisplay{ID: 12, UserID: 1, ColorKey: &green})
		after, err := store.FindUserFileEntry(ctx, 1, 13)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("展示读取改变了基础记录、版本或时间", err)
		}
	})
	t.Run("device rename and revocation preserve publication name", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET name='撤销后的设备名称',revoked_at=UTC_TIMESTAMP(6) WHERE id=13"); err != nil {
			t.Fatal(err)
		}
		check(t, ctx, store, file)
	})

	// 3. 核对普通一致性读取与已有只读快照入口的组合。
	t.Run("uncommitted update does not block ordinary read", func(t *testing.T) {
		transaction, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(ctx, "UPDATE file_entries SET extension='txt' WHERE id=13"); err != nil {
			t.Fatal(err)
		}
		readContext, release := context.WithTimeout(ctx, 500*time.Millisecond)
		defer release()
		check(t, readContext, store, file)
	})
	t.Run("base and display share one repeatable read snapshot", func(t *testing.T) {
		if err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, reader *Store) error {
			basic, err := reader.FindUserFileEntry(snapshotContext, 1, 13)
			if err != nil {
				return err
			}
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET extension='txt',revision=revision+1 WHERE id=13"); err != nil {
				return err
			}
			check(t, snapshotContext, reader, file)
			again, err := reader.FindUserFileEntry(snapshotContext, 1, 13)
			if err != nil || !reflect.DeepEqual(basic, again) {
				t.Fatal("同一快照中的基础记录出现变化", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		textExtension := "txt"
		file.Extension = &textExtension
		check(t, ctx, store, file)
	})

	// 4. 最大主键在普通夹具之后写入，核对整数和日期边界及故障分类。
	t.Run("full unsigned ids and database time boundaries", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','完整整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'edbf7e35-36ea-45a5-bdb9-58e76b46fd5e','完整整数设备','ANDROID')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		for _, boundary := range []struct {
			entryID  uint64
			parentID any
			name     string
			stamp    time.Time
		}{
			{entryID: maximum, stamp: time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)},
			{entryID: maximum - 1, parentID: maximum, name: "最大目录", stamp: time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)},
		} {
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,source_device_id,source_device_name,content_modified_at) VALUES(?,?,?,?,CAST(? AS BINARY),'DIR',?,'完整整数设备',?)", boundary.entryID, maximum, boundary.parentID, boundary.name, boundary.name, maximum, boundary.stamp); err != nil {
				t.Fatal(err)
			}
			name := "完整整数设备"
			check(t, ctx, store, domain.FileEntryDisplay{ID: boundary.entryID, UserID: maximum, SourceDeviceID: &maximum, SourceDeviceName: &name, ContentModifiedAt: &boundary.stamp})
		}
	})
	t.Run("closed pool is a database error", func(t *testing.T) {
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		result, err := store.FindUserFileEntryDisplay(ctx, 1, 13)
		if result != nil || err == nil || errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("连接故障被当作目录项不存在")
		}
	})
}
