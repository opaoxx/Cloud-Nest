package repository

import (
	"database/sql"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"github.com/opaoxx/cloud-nest/migrations"
	"gorm.io/gorm"
)

func TestNewStagedObjectMappingSchema(t *testing.T) {
	// 1. 用现有 Store 解析六列创建映射，核对正式表列定义。
	connector := &accountConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	statement := &gorm.Statement{DB: store.database}
	if err := statement.Parse(&models.NewStagedObject{}); err != nil {
		t.Fatal(err)
	}
	if statement.Table != "stored_objects" || len(statement.Schema.DBNames) != 6 || len(statement.Schema.Fields) != 6 || len(statement.Schema.PrimaryFields) != 0 {
		t.Fatal("暂存创建映射的表名、字段或标识约定错误")
	}
	_, table, found := strings.Cut(migrations.InitialSQL, "CREATE TABLE stored_objects (\n")
	if !found {
		t.Fatal("正式迁移缺少物理对象表")
	}
	table, _, _ = strings.Cut(table, ") ENGINE=")
	columnPattern := regexp.MustCompile(`(?m)^ ([a-z_][a-z_0-9]*) (BIGINT UNSIGNED|VARCHAR\(\d+\)|BINARY\(32\)|DATETIME\(6\))([^\n]*)`)
	columns := make(map[string][]string)
	for _, match := range columnPattern.FindAllStringSubmatch(table, -1) {
		columns[match[1]] = match
	}
	if len(columns) != 17 {
		t.Fatal("正式物理对象列提取不完整")
	}
	for _, fixture := range []struct {
		fieldName string
		column    string
		sqlType   string
		fieldType reflect.Type
	}{
		{"OwnerUserID", "owner_user_id", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"Provider", "provider", "VARCHAR(32)", reflect.TypeFor[string]()},
		{"Bucket", "bucket", "VARCHAR(63)", reflect.TypeFor[string]()},
		{"ObjectKey", "object_key", "VARCHAR(255)", reflect.TypeFor[string]()},
		{"FileSize", "file_size", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"Status", "status", "VARCHAR(16)", reflect.TypeFor[string]()},
	} {
		t.Run(fixture.column, func(t *testing.T) {
			// 2. 核对准确类型及可读、仅创建写入权限。
			field := statement.Schema.FieldsByDBName[fixture.column]
			column := columns[fixture.column]
			if field == nil || field.Name != fixture.fieldName || field.FieldType != fixture.fieldType ||
				field.TagSettings["COLUMN"] != fixture.column || !field.Readable || !field.Creatable || field.Updatable ||
				len(column) != 4 || column[2] != fixture.sqlType || !strings.Contains(column[3], "NOT NULL") {
				t.Fatal("暂存创建列、类型或字段权限错误")
			}
		})
	}
	// 3. 核对其余列可用默认/空值/生成值补齐，以及解析期间零连接。
	for name, column := range columns {
		if statement.Schema.FieldsByDBName[name] == nil && strings.Contains(column[3], "NOT NULL") &&
			!strings.Contains(column[3], "DEFAULT") && !strings.Contains(column[3], "AUTO_INCREMENT") && !strings.Contains(column[3], "GENERATED ALWAYS AS") {
			t.Fatal("暂存创建遗漏需要显式提供的列")
		}
	}
	if !strings.Contains(columns["status"][3], "DEFAULT 'STAGING'") || connector.connections.Load() != 0 || connector.pings.Load() != 0 {
		t.Fatal("暂存默认状态或解析连接行为错误")
	}
}

func TestNewStagedObjectMappingInsertSQL(t *testing.T) {
	for _, mode := range []string{"empty file", "ordinary file", "single limit", "above single limit", "maximum unsigned", "maximum text lengths", "original text"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 从暂存创建输入及可信用户标识准备六列记录。
			input := domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "users/7/objects/test"}
			userID := uint64(7)
			switch mode {
			case "ordinary file":
				input.SizeBytes = 2048
			case "single limit":
				input.SizeBytes = 16777216
			case "above single limit":
				input.SizeBytes = 16777217
			case "maximum unsigned":
				userID, input.SizeBytes = ^uint64(0), ^uint64(0)
			case "maximum text lengths":
				input.Provider, input.Bucket, input.ObjectKey = strings.Repeat("p", 32), strings.Repeat("b", 63), strings.Repeat("k", 255)
			case "original text":
				input.Provider, input.Bucket, input.ObjectKey = "Local_Config", "configured-bucket", "users/7/objects/Case_Sensitive-Key"
			}
			if !input.IsValid() {
				t.Fatal("暂存创建 SQL 测试输入无效")
			}
			record := models.NewStagedObject{
				OwnerUserID: userID, Provider: input.Provider, Bucket: input.Bucket, ObjectKey: input.ObjectKey,
				FileSize: input.SizeBytes, Status: string(domain.StoredObjectStatusStaging),
			}
			original := record
			connector := &accountConnector{}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			// 2. 使用 GORM DryRun 生成实际列清单及六个绑定参数。
			result := store.database.Session(&gorm.Session{DryRun: true, SkipDefaultTransaction: true}).Create(&record)
			expectedSQL := "INSERT INTO stored_objects (owner_user_id,provider,bucket,object_key,file_size,status) VALUES (?,?,?,?,?,?)"
			actualSQL := strings.ReplaceAll(result.Statement.SQL.String(), "`", "")
			expectedValues := []any{userID, input.Provider, input.Bucket, input.ObjectKey, input.SizeBytes, "STAGING"}
			if result.Error != nil || actualSQL != expectedSQL || !reflect.DeepEqual(result.Statement.Vars, expectedValues) || record != original {
				t.Fatal("暂存创建 SQL、完整参数或原值保护错误", result.Error)
			}
			// 3. 验证创建字段权限实际影响更新 SQL，生成期间零连接。
			update := store.database.Session(&gorm.Session{DryRun: true, SkipDefaultTransaction: true}).Model(&record).
				Where("owner_user_id = ? AND object_key = ?", userID, input.ObjectKey).
				Updates(map[string]any{"owner_user_id": userID, "provider": input.Provider, "bucket": input.Bucket,
					"object_key": input.ObjectKey, "file_size": input.SizeBytes, "status": "READY"})
			if update.Error != nil || update.Statement.SQL.Len() != 0 || connector.connections.Load() != 0 || connector.pings.Load() != 0 {
				t.Fatal("仅创建映射的更新权限或 SQL 生成连接行为错误", update.Error)
			}
		})
	}
}
