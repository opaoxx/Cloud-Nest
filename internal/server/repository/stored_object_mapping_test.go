package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
	"github.com/opaoxx/cloud-nest/migrations"
	"gorm.io/gorm"
)

// storedObjectMappingRows 复用单行读取，提供正式物理对象的十七列。
type storedObjectMappingRows struct{ *fileEntryRows }

func (*storedObjectMappingRows) Columns() []string {
	return []string{
		"id", "owner_user_id", "provider", "bucket", "object_key", "storage_version_id",
		"file_size", "verified_sha256", "status", "dedup_hash", "storage_etag", "verified_at",
		"unreferenced_at", "gc_after", "last_error_code", "created_at", "updated_at",
	}
}

func TestStoredObjectMappingSchema(t *testing.T) {
	// 1. 通过现有 Store 解析表名、字段及只读权限。
	connector := &accountConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	statement := &gorm.Statement{DB: store.database}
	if err := statement.Parse(&models.StoredObject{}); err != nil {
		t.Fatal(err)
	}
	if statement.Table != "stored_objects" || len(statement.Schema.DBNames) != 17 || len(statement.Schema.Fields) != 17 {
		t.Fatal("GORM 物理对象表名或字段数量错误")
	}
	_, table, found := strings.Cut(migrations.InitialSQL, "CREATE TABLE stored_objects (\n")
	if !found {
		t.Fatal("正式物理对象表未找到")
	}
	table, _, _ = strings.Cut(table, ") ENGINE=")
	columns := regexp.MustCompile(`(?m)^ ([a-z_][a-z_0-9]*) (?:BIGINT|VARCHAR|BINARY|DATETIME)`).FindAllStringSubmatch(table, -1)
	if len(columns) != 17 {
		t.Fatal("正式物理对象列提取不完整")
	}
	domainType := reflect.TypeFor[domain.StoredObject]()
	for _, column := range columns {
		t.Run(column[1], func(t *testing.T) {
			// 2. 对照正式列名、字段权限与领域类型的转换约定。
			field := statement.Schema.FieldsByDBName[column[1]]
			if field == nil || field.TagSettings["COLUMN"] != column[1] || !field.Readable || field.Creatable || field.Updatable {
				t.Fatal("物理对象列名或只读映射权限错误")
			}
			domainField, exists := domainType.FieldByName(field.Name)
			if !exists {
				t.Fatal("物理对象映射缺少对应领域字段")
			}
			expectedType := domainField.Type
			if field.Name == "VerifiedSHA256" || field.Name == "DedupHash" {
				expectedType = reflect.TypeFor[[]byte]()
			}
			if field.Name == "Status" {
				expectedType = reflect.TypeFor[string]()
			}
			if field.FieldType != expectedType {
				t.Fatal("物理对象映射字段类型与领域衔接约定不符")
			}
		})
	}
	// 3. 确认解析仅处理映射信息。
	if connector.connections.Load() != 0 || connector.pings.Load() != 0 {
		t.Fatal("解析物理对象映射时触发数据库连接")
	}
}

func TestStoredObjectMappingScan(t *testing.T) {
	for _, mode := range []string{
		"complete row", "SQL NULL optional fields", "explicit empty values", "maximum unsigned numbers",
		"zero digests", "short verified digest retained", "short dedup digest retained", "unknown status retained",
		"time boundaries", "negative id", "overflow id", "negative owner", "negative size", "invalid time",
	} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备十七列数据，分别覆盖正常、可空及扫描边界。
			stamp := time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC)
			gcAfter := stamp.Add(7 * 24 * time.Hour)
			versionID, storageETag, errorCode := "storage-version-1", `"opaque-etag-1"`, "STORAGE_RETRY"
			digest := make([]byte, 32)
			for index := range digest {
				digest[index] = byte(index * 7)
			}
			want := models.StoredObject{
				ID: 23, OwnerUserID: 7, Provider: "local_minio", Bucket: "cloudnest",
				ObjectKey: "uploads/test-attempt/object", StorageVersionID: &versionID, FileSize: 2048,
				VerifiedSHA256: digest, Status: "READY", DedupHash: append([]byte(nil), digest...),
				StorageETag: &storageETag, VerifiedAt: &stamp, UnreferencedAt: &stamp,
				GCAfter: &gcAfter, LastErrorCode: &errorCode, CreatedAt: stamp, UpdatedAt: stamp.Add(time.Second),
			}
			values := []driver.Value{
				int64(23), int64(7), want.Provider, want.Bucket, want.ObjectKey, versionID,
				int64(2048), want.VerifiedSHA256, want.Status, want.DedupHash, storageETag, stamp,
				stamp, gcAfter, errorCode, want.CreatedAt, want.UpdatedAt,
			}
			switch mode {
			case "SQL NULL optional fields":
				for _, column := range []int{5, 7, 9, 10, 11, 12, 13, 14} {
					values[column] = nil
				}
				want.StorageVersionID, want.VerifiedSHA256, want.DedupHash, want.StorageETag = nil, nil, nil, nil
				want.VerifiedAt, want.UnreferencedAt, want.GCAfter, want.LastErrorCode = nil, nil, nil, nil
				want.Status, values[8] = "STAGING", "STAGING"
			case "explicit empty values":
				versionID, storageETag, errorCode = "", "", ""
				for _, column := range []int{5, 10, 14} {
					values[column] = ""
				}
				want.VerifiedSHA256, values[7] = []byte{}, []byte{}
				want.DedupHash, values[9] = []byte{}, []byte{}
				want.FileSize, values[6] = 0, int64(0)
			case "maximum unsigned numbers":
				want.ID, want.OwnerUserID, want.FileSize = ^uint64(0), ^uint64(0), ^uint64(0)
				for _, column := range []int{0, 1, 6} {
					values[column] = "18446744073709551615"
				}
			case "zero digests":
				want.VerifiedSHA256, values[7] = make([]byte, 32), make([]byte, 32)
				want.DedupHash, values[9] = make([]byte, 32), make([]byte, 32)
			case "short verified digest retained":
				want.VerifiedSHA256, values[7] = make([]byte, 31), make([]byte, 31)
			case "short dedup digest retained":
				want.DedupHash, values[9] = make([]byte, 31), make([]byte, 31)
			case "unknown status retained":
				want.Status, values[8] = "ready ", "ready "
			case "time boundaries":
				want.CreatedAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				want.UpdatedAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
				values[15], values[16] = want.CreatedAt, want.UpdatedAt
			case "negative id":
				values[0] = int64(-1)
			case "overflow id":
				values[0] = "18446744073709551616"
			case "negative owner":
				values[1] = int64(-1)
			case "negative size":
				values[6] = int64(-1)
			case "invalid time":
				values[11] = "invalid-time"
			}
			// 2. 复用 SQL 测试连接器，通过 GORM 扫描对象记录。
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				actualSQL := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actualSQL != "SELECT * FROM stored_objects WHERE owner_user_id = ? AND id = ? LIMIT ?" ||
					len(arguments) != 3 || arguments[0].Value != want.OwnerUserID || arguments[1].Value != want.ID || arguments[2].Value != int64(1) {
					t.Error("物理对象映射表名、范围参数或结果上限错误")
				}
				return &storedObjectMappingRows{fileEntryRows: &fileEntryRows{values: values}}, nil
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			var actual models.StoredObject
			err = store.database.WithContext(context.Background()).Where("owner_user_id = ? AND id = ?", want.OwnerUserID, want.ID).Take(&actual).Error
			// 3. 核对完整结果，并确认异常数值或时间产生扫描错误。
			scanFailure := strings.HasPrefix(mode, "negative ") || mode == "overflow id" || mode == "invalid time"
			if scanFailure {
				if err == nil {
					t.Fatal("非法数值或时间未触发扫描错误")
				}
			} else if err != nil || !reflect.DeepEqual(actual, want) {
				t.Fatal("物理对象十七字段未完整扫描", err)
			}
			if queries != 1 {
				t.Fatal("物理对象映射扫描未恰好执行一次查询")
			}
		})
	}
}
