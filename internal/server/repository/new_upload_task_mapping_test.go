package repository

import (
	"bytes"
	"database/sql"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"github.com/opaoxx/cloud-nest/migrations"
	"gorm.io/gorm"
)

func TestNewUploadTaskMappingSchema(t *testing.T) {
	// 1. 解析十三列创建映射，提取正式上传任务的二十八列。
	connector := &accountConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	statement := &gorm.Statement{DB: store.database}
	if err := statement.Parse(&models.NewUploadTask{}); err != nil {
		t.Fatal(err)
	}
	if statement.Table != "upload_tasks" || len(statement.Schema.DBNames) != 13 || len(statement.Schema.Fields) != 13 || len(statement.Schema.PrimaryFields) != 0 {
		t.Fatal("上传任务创建映射的表名、列数或标识约定错误")
	}
	_, table, found := strings.Cut(migrations.InitialSQL, "CREATE TABLE upload_tasks (\n")
	if !found {
		t.Fatal("正式上传任务表未找到")
	}
	table, _, _ = strings.Cut(table, ") ENGINE=")
	columnPattern := regexp.MustCompile(`(?m)^ ([a-z_][a-z_0-9]*) (BIGINT UNSIGNED|INT UNSIGNED|VARCHAR\(\d+\)|BINARY\(32\)|DATETIME\(6\))([^\n]*)`)
	columns := make(map[string][]string)
	for _, match := range columnPattern.FindAllStringSubmatch(table, -1) {
		columns[match[1]] = match
	}
	if len(columns) != 28 {
		t.Fatal("正式任务列提取不完整")
	}
	for _, fixture := range []struct {
		fieldName string
		column    string
		sqlType   string
		fieldType reflect.Type
		nullable  bool
	}{
		{"UserID", "user_id", "BIGINT UNSIGNED", reflect.TypeFor[uint64](), false},
		{"DeviceID", "device_id", "BIGINT UNSIGNED", reflect.TypeFor[uint64](), false},
		{"StagedObjectID", "staged_object_id", "BIGINT UNSIGNED", reflect.TypeFor[uint64](), false},
		{"TargetParentID", "target_parent_id", "BIGINT UNSIGNED", reflect.TypeFor[uint64](), false},
		{"RequestedName", "requested_name", "VARCHAR(120)", reflect.TypeFor[string](), false},
		{"UploadMode", "upload_mode", "VARCHAR(16)", reflect.TypeFor[string](), false},
		{"ExpectedSHA256", "expected_sha256", "BINARY(32)", reflect.TypeFor[[]byte](), true},
		{"FileSize", "file_size", "BIGINT UNSIGNED", reflect.TypeFor[uint64](), false},
		{"PartSize", "part_size", "BIGINT UNSIGNED", reflect.TypeFor[*uint64](), true},
		{"PartCount", "part_count", "INT UNSIGNED", reflect.TypeFor[*uint32](), true},
		{"Status", "status", "VARCHAR(16)", reflect.TypeFor[string](), false},
		{"ReservedBytes", "reserved_bytes", "BIGINT UNSIGNED", reflect.TypeFor[uint64](), false},
		{"ExpiresAt", "expires_at", "DATETIME(6)", reflect.TypeFor[time.Time](), false},
	} {
		t.Run(fixture.column, func(t *testing.T) {
			// 2. 核对列名、类型、NULL 语义及仅创建写入权限。
			field := statement.Schema.FieldsByDBName[fixture.column]
			column := columns[fixture.column]
			if field == nil || field.Name != fixture.fieldName || field.FieldType != fixture.fieldType ||
				field.TagSettings["COLUMN"] != fixture.column || !field.Readable || !field.Creatable || field.Updatable ||
				len(column) != 4 || column[2] != fixture.sqlType || (!strings.Contains(column[3], "NOT NULL")) != fixture.nullable {
				t.Fatal("任务创建字段、类型、空值或权限错误")
			}
		})
	}
	// 3. 其余列须有自增、默认或空值语义，解析阶段保持零连接。
	for name, column := range columns {
		if statement.Schema.FieldsByDBName[name] == nil && strings.Contains(column[3], "NOT NULL") &&
			!strings.Contains(column[3], "DEFAULT") && !strings.Contains(column[3], "AUTO_INCREMENT") {
			t.Fatal("任务创建遗漏需要显式提供的列")
		}
	}
	if connector.connections.Load() != 0 || connector.pings.Load() != 0 {
		t.Fatal("任务创建映射解析触发数据库连接")
	}
}

func TestNewUploadTaskMappingInsertSQL(t *testing.T) {
	for _, mode := range []string{
		"zero-byte without digest", "ordinary file", "single limit", "above single limit", "declared digest",
		"zero digest", "maximum unsigned", "Unicode name boundary", "expiry boundaries", "multipart plan",
	} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备创建字段，覆盖可空摘要、完整数值、名称及日期。
			record := models.NewUploadTask{
				UserID: 7, DeviceID: 11, StagedObjectID: 100, TargetParentID: 13, RequestedName: "学习笔记.MD",
				UploadMode: "SINGLE", Status: "UPLOADING", ExpiresAt: time.Date(2026, 11, 18, 8, 0, 0, 123456000, time.UTC),
			}
			switch mode {
			case "ordinary file":
				record.FileSize = 2048
			case "single limit":
				record.FileSize = 16777216
			case "above single limit":
				record.FileSize = 16777217
			case "declared digest":
				record.FileSize, record.ExpectedSHA256 = 1, bytes.Repeat([]byte{0xab}, 32)
			case "zero digest":
				record.ExpectedSHA256 = make([]byte, 32)
			case "maximum unsigned":
				record.UserID, record.DeviceID, record.StagedObjectID, record.TargetParentID, record.FileSize = ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)
			case "Unicode name boundary":
				record.RequestedName = strings.Repeat("🌱", 60)
			case "expiry boundaries":
				record.ExpiresAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
			case "multipart plan":
				record.UploadMode, record.Status = "MULTIPART", "CREATING"
				record.FileSize, record.ExpectedSHA256 = 16777217, bytes.Repeat([]byte{0xab}, 32)
				partSize, partCount := uint64(16777216), uint32(2)
				record.PartSize, record.PartCount = &partSize, &partCount
			}
			record.ReservedBytes = record.FileSize
			original := record
			original.ExpectedSHA256 = bytes.Clone(record.ExpectedSHA256)
			connector := &accountConnector{}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			// 2. 核对两种模式的真实 GORM INSERT 列清单和原值参数。
			insert := store.database.Session(&gorm.Session{DryRun: true, SkipDefaultTransaction: true})
			expectedSQL := "INSERT INTO upload_tasks (user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,expected_sha256,file_size,status,reserved_bytes,expires_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)"
			expectedValues := []any{record.UserID, record.DeviceID, record.StagedObjectID, record.TargetParentID, record.RequestedName,
				record.UploadMode, record.ExpectedSHA256, record.FileSize, record.Status, record.ReservedBytes, record.ExpiresAt}
			if mode == "multipart plan" {
				expectedSQL = "INSERT INTO upload_tasks (user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)"
				expectedValues = []any{record.UserID, record.DeviceID, record.StagedObjectID, record.TargetParentID, record.RequestedName,
					record.UploadMode, record.ExpectedSHA256, record.FileSize, record.PartSize, record.PartCount, record.Status, record.ReservedBytes, record.ExpiresAt}
			} else {
				insert = insert.Omit("part_size", "part_count")
			}
			result := insert.Create(&record)
			if result.Error != nil || strings.ReplaceAll(result.Statement.SQL.String(), "`", "") != expectedSQL ||
				!reflect.DeepEqual(result.Statement.Vars, expectedValues) || !reflect.DeepEqual(record, original) {
				t.Fatal("上传任务创建 SQL、完整参数或来源保护错误", result.Error)
			}
			// 3. 核对全部字段的更新 SQL 权限以及生成阶段零连接。
			changes := make(map[string]any)
			for _, column := range result.Statement.Schema.DBNames {
				changes[column] = nil
			}
			update := store.database.Session(&gorm.Session{DryRun: true, SkipDefaultTransaction: true}).Model(&record).
				Where("user_id = ? AND staged_object_id = ?", record.UserID, record.StagedObjectID).Updates(changes)
			if update.Error != nil || update.Statement.SQL.Len() != 0 || connector.connections.Load() != 0 || connector.pings.Load() != 0 {
				t.Fatal("任务仅创建权限或 SQL 生成连接行为错误", update.Error)
			}
		})
	}
}
