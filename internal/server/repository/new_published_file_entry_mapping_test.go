package repository

import (
	"database/sql"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"github.com/opaoxx/cloud-nest/migrations"
	"gorm.io/gorm"
)

func publishedFileEntryMappingFixture() models.NewPublishedFileEntry {
	extension := "pdf"
	return models.NewPublishedFileEntry{
		UserID: 7, ParentID: 13, Name: "Cafe\u0301 (1).PDF", NameKey: []byte("café (1).pdf"),
		EntryType: "FILE", Status: "ACTIVE", Category: "DOCUMENT", Extension: &extension,
		ObjectID: 29, FileSize: domain.MaxSingleUploadSizeBytes, SourceDeviceID: 11, SourceDeviceName: "fixture",
		ContentModifiedAt: time.Date(2026, 11, 19, 8, 0, 0, 123456000, time.UTC),
	}
}

func TestNewPublishedFileEntryMappingSchema(t *testing.T) {
	pool := sql.OpenDB(&accountConnector{})
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	// 1. 以 GORM 解析正式映射，核对目标表及写入范围。
	statement := &gorm.Statement{DB: store.database}
	if err := statement.Parse(&models.NewPublishedFileEntry{}); err != nil {
		t.Fatal(err)
	}
	if statement.Table != "file_entries" || len(statement.Schema.Fields) != 13 ||
		len(statement.Schema.DBNames) != 13 || len(statement.Schema.PrimaryFields) != 0 {
		t.Fatal("发布文件映射的表名、字段或自增 ID 约定错误")
	}
	_, table, found := strings.Cut(migrations.InitialSQL, "CREATE TABLE file_entries (\n")
	if !found {
		t.Fatal("正式迁移缺少目录项表")
	}
	table, _, _ = strings.Cut(table, ") ENGINE=")

	// 2. 核对字段、准确列类型及仅创建写入权限。
	for _, column := range []struct {
		goName, sqlName, sqlType string
		goType                   reflect.Type
	}{
		{"UserID", "user_id", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"ParentID", "parent_id", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"Name", "name", "VARCHAR(120)", reflect.TypeFor[string]()},
		{"NameKey", "name_key", "VARBINARY(768)", reflect.TypeFor[[]byte]()},
		{"EntryType", "entry_type", "VARCHAR(8)", reflect.TypeFor[string]()},
		{"Status", "status", "VARCHAR(16)", reflect.TypeFor[string]()},
		{"Category", "category", "VARCHAR(16)", reflect.TypeFor[string]()},
		{"Extension", "extension", "VARCHAR(32)", reflect.TypeFor[*string]()},
		{"ObjectID", "object_id", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"FileSize", "file_size", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"SourceDeviceID", "source_device_id", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"SourceDeviceName", "source_device_name", "VARCHAR(100)", reflect.TypeFor[string]()},
		{"ContentModifiedAt", "content_modified_at", "DATETIME(6)", reflect.TypeFor[time.Time]()},
	} {
		field := statement.Schema.FieldsByDBName[column.sqlName]
		if field == nil || field.Name != column.goName || field.FieldType != column.goType ||
			!field.Readable || !field.Creatable || field.Updatable || field.AutoCreateTime != 0 ||
			!strings.Contains("\n"+table, "\n "+column.sqlName+" "+column.sqlType+" ") {
			t.Fatal("发布文件列类型或权限错误", column.sqlName)
		}
	}
}

func TestNewPublishedFileEntryMappingInsertSQL(t *testing.T) {
	connector := &accountConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	expectedSQL := "INSERT INTO file_entries (user_id,parent_id,name,name_key,entry_type,status,category,extension,object_id,file_size,source_device_id,source_device_name,content_modified_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)"
	for _, scenario := range []string{"ordinary", "NULL extension and zero bytes", "maximum unsigned values"} {
		t.Run(scenario, func(t *testing.T) {
			record := publishedFileEntryMappingFixture()
			switch scenario {
			case "NULL extension and zero bytes":
				record.Name, record.NameKey, record.Category, record.Extension, record.FileSize = ".env", []byte(".env"), "OTHER", nil, 0
			case "maximum unsigned values":
				record.UserID, record.ParentID, record.ObjectID, record.FileSize, record.SourceDeviceID =
					math.MaxUint64, math.MaxUint64, math.MaxUint64, math.MaxUint64, math.MaxUint64
			}
			result := store.database.Session(&gorm.Session{DryRun: true, SkipDefaultTransaction: true}).Create(&record)
			expectedValues := []any{record.UserID, record.ParentID, record.Name, record.NameKey, "FILE", "ACTIVE",
				record.Category, record.Extension, record.ObjectID, record.FileSize, record.SourceDeviceID, record.SourceDeviceName, record.ContentModifiedAt}
			if result.Error != nil || strings.ReplaceAll(result.Statement.SQL.String(), "`", "") != expectedSQL ||
				!reflect.DeepEqual(result.Statement.Vars, expectedValues) || connector.connections.Load() != 0 {
				t.Fatal("发布文件 INSERT 列或绑定参数错误", result.Statement.SQL.String(), result.Statement.Vars, result.Error)
			}
		})
	}
}
