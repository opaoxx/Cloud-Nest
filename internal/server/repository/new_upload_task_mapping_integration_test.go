package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

func TestNewUploadTaskMappingIntegration(t *testing.T) {
	// 1. 沿用正式上传任务表及独占库，准备完整读取入口。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Date(2026, 11, 18, 8, 0, 0, 123456000, time.UTC)
	newRecord := func(objectID uint64) models.NewUploadTask {
		return models.NewUploadTask{UserID: 7, DeviceID: 11, StagedObjectID: objectID, TargetParentID: 13,
			RequestedName: "学习笔记.MD", UploadMode: "SINGLE", Status: "UPLOADING", ExpiresAt: expiresAt}
	}
	prepareObject := func(record models.NewUploadTask) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket',?,?)",
			record.StagedObjectID, record.UserID, fmt.Sprintf("mapping-upload-%d", record.StagedObjectID), record.FileSize); err != nil {
			t.Fatal(err)
		}
	}
	checkInsert := func(t *testing.T, record models.NewUploadTask) {
		t.Helper()
		// 2. 插入十一列，再按用户及唯一暂存对象取得任务标识。
		original := record
		original.ExpectedSHA256 = bytes.Clone(record.ExpectedSHA256)
		result := store.database.WithContext(ctx).Create(&record)
		if result.Error != nil || result.RowsAffected != 1 || !reflect.DeepEqual(record, original) {
			t.Fatal("真实上传任务映射插入或来源保护错误", result.Error)
		}
		var taskID uint64
		if err := pool.QueryRowContext(ctx, "SELECT id FROM upload_tasks WHERE user_id=? AND staged_object_id=?", record.UserID, record.StagedObjectID).Scan(&taskID); err != nil || taskID == 0 {
			t.Fatal("按用户及暂存对象回读任务标识失败", err)
		}
		actual, err := store.FindUserUploadTask(ctx, record.UserID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		// 3. 核对声明、初始空值、租约/重试默认值及自动时间。
		var expectedSHA256 *[sha256.Size]byte
		if record.ExpectedSHA256 != nil {
			expectedSHA256 = new([sha256.Size]byte)
			copy(expectedSHA256[:], record.ExpectedSHA256)
		}
		want := domain.UploadTask{
			ID: taskID, UserID: record.UserID, DeviceID: record.DeviceID, StagedObjectID: record.StagedObjectID,
			TargetParentID: record.TargetParentID, RequestedName: record.RequestedName, UploadMode: "SINGLE",
			ExpectedSHA256: expectedSHA256, FileSize: record.FileSize, Status: domain.UploadStatusUploading,
			ReservedBytes: record.ReservedBytes, ExpiresAt: record.ExpiresAt, CreatedAt: actual.CreatedAt, UpdatedAt: actual.UpdatedAt,
		}
		if !reflect.DeepEqual(actual, &want) || actual.CreatedAt.IsZero() || actual.CreatedAt != actual.UpdatedAt || actual.CreatedAt.Location() != time.UTC {
			t.Fatal("任务创建字段、空值、默认数值或 UTC 时间错误")
		}
	}
	for index, mode := range []string{"zero-byte without digest", "ordinary file", "single limit", "declared digest", "zero digest", "Unicode name boundary"} {
		t.Run(mode, func(t *testing.T) {
			record := newRecord(uint64(100 + index))
			switch mode {
			case "ordinary file":
				record.FileSize = 2048
			case "single limit":
				record.FileSize = 16777216
			case "declared digest":
				record.FileSize, record.ExpectedSHA256 = 1, bytes.Repeat([]byte{0xab}, 32)
			case "zero digest":
				record.ExpectedSHA256 = make([]byte, 32)
			case "Unicode name boundary":
				record.RequestedName = strings.Repeat("🌱", 60)
			}
			record.ReservedBytes = record.FileSize
			prepareObject(record)
			checkInsert(t, record)
		})
	}
	t.Run("unique ownership and lifecycle constraints", func(t *testing.T) {
		for index, fixture := range []struct {
			name   string
			number uint16
		}{
			{"duplicate staged object", 1062}, {"foreign device", 1452}, {"foreign staged object", 1452}, {"foreign parent", 1452},
			{"multipart without parts", 3819}, {"completed without result", 3819}, {"excess reservation", 3819}, {"unknown status", 3819},
		} {
			record := newRecord(uint64(300 + index))
			prepareObject(record)
			switch fixture.name {
			case "duplicate staged object":
				record.StagedObjectID = 100
			case "foreign device":
				record.DeviceID = 12
			case "foreign staged object":
				record.StagedObjectID = 24
			case "foreign parent":
				record.TargetParentID = 14
			case "multipart without parts":
				record.UploadMode = "MULTIPART"
			case "completed without result":
				record.Status = "COMPLETED"
			case "excess reservation":
				record.ReservedBytes = 1
			case "unknown status":
				record.Status = "UNKNOWN"
			}
			err := store.database.WithContext(ctx).Create(&record).Error
			var databaseError *mysql.MySQLError
			if !errors.As(err, &databaseError) || databaseError.Number != fixture.number {
				t.Fatal("任务映射的正式数据库约束错误", fixture.name, err)
			}
		}
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM upload_tasks").Scan(&count); err != nil || count != 7 {
			t.Fatal("失败插入改变了任务记录数", err)
		}
	})

	// 4. 最后准备高位身份与自增值，核对全部无符号参数及日期上界。
	t.Run("unsigned references generated id and expiry", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'mapping_upload_max','完整整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'48ee758a-0727-42de-ae7d-3bb33155e148','完整整数设备','WINDOWS')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "ALTER TABLE upload_tasks AUTO_INCREMENT = 9223372036854775810"); err != nil {
			t.Fatal(err)
		}
		record := newRecord(maximum)
		record.UserID, record.DeviceID, record.TargetParentID, record.FileSize, record.ReservedBytes = maximum, maximum, maximum, maximum, maximum
		record.ExpiresAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		prepareObject(record)
		checkInsert(t, record)
		var taskID uint64
		if err := pool.QueryRowContext(ctx, "SELECT id FROM upload_tasks WHERE user_id=? AND staged_object_id=?", maximum, maximum).Scan(&taskID); err != nil || taskID < uint64(1)<<63 {
			t.Fatal("高位自增任务标识回读错误", err)
		}
	})
}
