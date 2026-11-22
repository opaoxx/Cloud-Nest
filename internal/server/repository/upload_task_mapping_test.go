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

// uploadTaskMappingRows 复用单行结果读取，提供正式上传任务的完整列。
type uploadTaskMappingRows struct{ *fileEntryRows }

func (*uploadTaskMappingRows) Columns() []string {
	return []string{
		"id", "user_id", "device_id", "staged_object_id", "published_object_id", "target_parent_id",
		"requested_name", "resolved_name", "upload_mode", "storage_upload_id", "expected_sha256",
		"file_size", "part_size", "part_count", "status", "result_entry_id", "reserved_bytes",
		"lease_owner", "lease_until", "lease_version", "next_attempt_at", "attempt_count", "expires_at",
		"last_error_code", "last_error_message", "created_at", "updated_at", "completed_at",
		"replacement_entry_id", "replacement_expected_revision", "replacement_published_revision", "replacement_conflicting_revision",
	}
}

func TestUploadTaskMappingSchema(t *testing.T) {
	// 1. 核对表名、32 个列名和字段读取权限。
	connector := &accountConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	statement := &gorm.Statement{DB: store.database}
	if err := statement.Parse(&models.UploadTask{}); err != nil {
		t.Fatal(err)
	}
	if statement.Table != "upload_tasks" || len(statement.Schema.DBNames) != 32 || len(statement.Schema.Fields) != 32 {
		t.Fatal("GORM 上传任务表名或字段数量错误")
	}
	_, table, found := strings.Cut(migrations.InitialSQL, "CREATE TABLE upload_tasks (\n")
	if !found {
		t.Fatal("正式上传任务表未找到")
	}
	table, _, _ = strings.Cut(table, ") ENGINE=")
	columnPattern := regexp.MustCompile(`(?m)^ (?:ADD COLUMN )?([a-z_][a-z_0-9]*) (?:BIGINT|INT|VARCHAR|BINARY|DATETIME)`)
	columns := columnPattern.FindAllStringSubmatch(table+"\n"+migrations.Ordered()[4].SQL, -1)
	if len(columns) != 32 {
		t.Fatal("正式上传任务列提取不完整")
	}
	domainType := reflect.TypeFor[domain.UploadTask]()
	for _, column := range columns {
		t.Run(column[1], func(t *testing.T) {
			field := statement.Schema.FieldsByDBName[column[1]]
			if field == nil || field.TagSettings["COLUMN"] != column[1] || !field.Readable || field.Creatable || field.Updatable {
				t.Fatal("上传列名或只读映射权限错误")
			}
			domainField, exists := domainType.FieldByName(field.Name)
			if !exists {
				t.Fatal("上传映射缺少对应领域字段")
			}
			expectedType := domainField.Type
			if field.Name == "ExpectedSHA256" {
				expectedType = reflect.TypeFor[[]byte]()
			}
			if field.Name == "Status" {
				expectedType = reflect.TypeFor[string]()
			}
			if field.FieldType != expectedType {
				t.Fatal("上传映射字段类型与领域衔接约定不符")
			}
		})
	}
	if connector.connections.Load() != 0 || connector.pings.Load() != 0 {
		t.Fatal("解析上传映射时触发了数据库连接")
	}
}

func TestUploadTaskMappingScan(t *testing.T) {
	// 1. 准备完整任务数据及可空字段。
	for _, mode := range []string{
		"complete row", "SQL NULL optional fields", "explicit empty values", "maximum unsigned numbers",
		"zero digest", "short digest retained", "time boundaries", "negative id", "overflow id",
		"overflow part count", "negative attempt count", "invalid time",
	} {
		t.Run(mode, func(t *testing.T) {
			stamp := time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC)
			publishedID, resultID := uint64(29), uint64(31)
			resolvedName, storageID := "发布笔记.MD", "storage-session"
			partSize, partCount := uint64(5242880), uint32(3)
			leaseOwner, errorCode, errorMessage := "worker-1", "STORAGE_RETRY", "存储暂时不可用"
			digest := make([]byte, 32)
			for index := range digest {
				digest[index] = byte(index * 7)
			}
			want := models.UploadTask{
				ID: 19, UserID: 7, DeviceID: 11, StagedObjectID: 23, PublishedObjectID: &publishedID,
				TargetParentID: 13, RequestedName: "学习笔记.MD", ResolvedName: &resolvedName,
				UploadMode: "MULTIPART", StorageUploadID: &storageID, ExpectedSHA256: digest,
				FileSize: 16777216, PartSize: &partSize, PartCount: &partCount, Status: "VERIFYING",
				ResultEntryID: &resultID, ReservedBytes: 16777216,
				LeaseOwner: &leaseOwner, LeaseUntil: &stamp, LeaseVersion: 3, NextAttemptAt: &stamp,
				AttemptCount: 2, ExpiresAt: stamp.Add(24 * time.Hour), LastErrorCode: &errorCode, LastErrorMessage: &errorMessage,
				CreatedAt: stamp, UpdatedAt: stamp, CompletedAt: &stamp,
			}
			values := []driver.Value{
				int64(19), int64(7), int64(11), int64(23), int64(29), int64(13),
				want.RequestedName, resolvedName, want.UploadMode, storageID, digest,
				int64(16777216), int64(5242880), int64(3), "VERIFYING", int64(31), int64(16777216),
				leaseOwner, stamp, int64(3), stamp, int64(2), want.ExpiresAt,
				errorCode, errorMessage, stamp, stamp, stamp,
				nil, nil, nil, nil,
			}
			switch mode {
			case "SQL NULL optional fields":
				for _, column := range []int{4, 7, 9, 10, 12, 13, 15, 17, 18, 20, 23, 24, 27} {
					values[column] = nil
				}
				want.PublishedObjectID, want.ResolvedName, want.StorageUploadID, want.ExpectedSHA256 = nil, nil, nil, nil
				want.PartSize, want.PartCount, want.ResultEntryID = nil, nil, nil
				want.LeaseOwner, want.LeaseUntil, want.NextAttemptAt = nil, nil, nil
				want.LastErrorCode, want.LastErrorMessage, want.CompletedAt = nil, nil, nil
			case "explicit empty values":
				resolvedName, storageID, leaseOwner, errorCode, errorMessage = "", "", "", "", ""
				publishedID, resultID, partSize, partCount = 0, 0, 0, 0
				for _, column := range []int{7, 9, 17, 23, 24} {
					values[column] = ""
				}
				for _, column := range []int{4, 12, 13, 15} {
					values[column] = int64(0)
				}
				want.ExpectedSHA256, values[10] = []byte{}, []byte{}
			case "maximum unsigned numbers":
				maximum := ^uint64(0)
				want.ID, want.UserID, want.DeviceID, want.StagedObjectID, want.TargetParentID = maximum, maximum, maximum, maximum, maximum
				publishedID, resultID, partSize = maximum, maximum, maximum
				want.FileSize, want.ReservedBytes, want.LeaseVersion, want.AttemptCount = maximum, maximum, maximum, ^uint32(0)
				for _, column := range []int{0, 1, 2, 3, 4, 5, 11, 12, 15, 16, 19} {
					values[column] = "18446744073709551615"
				}
				partCount, values[13], values[21] = 10000, int64(10000), "4294967295"
				want.ReplacementEntryID, want.ReplacementExpectedRevision = &maximum, &maximum
				want.ReplacementPublishedRevision, want.ReplacementConflictingRevision = &maximum, &maximum
				for _, column := range []int{28, 29, 30, 31} {
					values[column] = "18446744073709551615"
				}
			case "zero digest":
				want.ExpectedSHA256, values[10] = make([]byte, 32), make([]byte, 32)
			case "short digest retained":
				want.ExpectedSHA256, values[10] = make([]byte, 31), make([]byte, 31)
			case "time boundaries":
				want.CreatedAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				want.UpdatedAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
				values[25], values[26] = want.CreatedAt, want.UpdatedAt
			case "negative id":
				values[0] = int64(-1)
			case "overflow id":
				values[0] = "18446744073709551616"
			case "overflow part count":
				values[13] = "4294967296"
			case "negative attempt count":
				values[21] = int64(-1)
			case "invalid time":
				values[22] = "invalid-time"
			}
			// 2. 通过现有 SQL 测试连接器扫描任务数据。
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				actualSQL := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actualSQL != "SELECT * FROM upload_tasks WHERE user_id = ? AND id = ? LIMIT ?" ||
					len(arguments) != 3 || arguments[0].Value != want.UserID || arguments[1].Value != want.ID || arguments[2].Value != int64(1) {
					t.Error("上传映射表名、范围参数或结果上限错误")
				}
				return &uploadTaskMappingRows{fileEntryRows: &fileEntryRows{values: values}}, nil
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			var actual models.UploadTask
			err = store.database.WithContext(context.Background()).Where("user_id = ? AND id = ?", want.UserID, want.ID).Take(&actual).Error
			// 3. 检查完整无符号数、可空字段、摘要和时间。
			scanFailure := strings.HasPrefix(mode, "negative ") || strings.HasPrefix(mode, "overflow ") || mode == "invalid time"
			if scanFailure {
				if err == nil {
					t.Fatal("非法数值或时间未触发扫描错误")
				}
			} else if err != nil || !reflect.DeepEqual(actual, want) {
				t.Fatal("上传任务字段未完整扫描", err)
			}
			if queries != 1 {
				t.Fatal("上传任务映射扫描未恰好执行一次查询")
			}
		})
	}
}
