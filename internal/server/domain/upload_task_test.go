package domain_test

import (
	"crypto/sha256"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestUploadTaskRecordShape(t *testing.T) {
	// 1. 从正式迁移提取上传任务列，准备领域字段及准确类型。
	_, table, found := strings.Cut(migrations.InitialSQL, "CREATE TABLE upload_tasks (\n")
	if !found {
		t.Fatal("正式迁移缺少上传任务表")
	}
	table, _, found = strings.Cut(table, ") ENGINE=")
	if !found {
		t.Fatal("上传任务表定义不完整")
	}
	columnPattern := regexp.MustCompile(`(?m)^ ([a-z_][a-z_0-9]*) (BIGINT UNSIGNED|INT UNSIGNED|VARCHAR\(\d+\)|BINARY\(32\)|DATETIME\(6\))([^\n]*)`)
	columns := make(map[string][]string)
	for _, match := range columnPattern.FindAllStringSubmatch(table, -1) {
		columns[match[1]] = match
	}
	fixtures := []struct {
		fieldName string
		column    string
		sqlType   string
		fieldType reflect.Type
	}{
		{"ID", "id", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"UserID", "user_id", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"DeviceID", "device_id", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"StagedObjectID", "staged_object_id", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"PublishedObjectID", "published_object_id", "BIGINT UNSIGNED", reflect.TypeFor[*uint64]()},
		{"TargetParentID", "target_parent_id", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"RequestedName", "requested_name", "VARCHAR(120)", reflect.TypeFor[string]()},
		{"ResolvedName", "resolved_name", "VARCHAR(120)", reflect.TypeFor[*string]()},
		{"UploadMode", "upload_mode", "VARCHAR(16)", reflect.TypeFor[string]()},
		{"StorageUploadID", "storage_upload_id", "VARCHAR(1024)", reflect.TypeFor[*string]()},
		{"ExpectedSHA256", "expected_sha256", "BINARY(32)", reflect.TypeFor[*[sha256.Size]byte]()},
		{"FileSize", "file_size", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"PartSize", "part_size", "BIGINT UNSIGNED", reflect.TypeFor[*uint64]()},
		{"PartCount", "part_count", "INT UNSIGNED", reflect.TypeFor[*uint32]()},
		{"Status", "status", "VARCHAR(16)", reflect.TypeFor[domain.UploadStatus]()},
		{"ResultEntryID", "result_entry_id", "BIGINT UNSIGNED", reflect.TypeFor[*uint64]()},
		{"ReservedBytes", "reserved_bytes", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"LeaseOwner", "lease_owner", "VARCHAR(64)", reflect.TypeFor[*string]()},
		{"LeaseUntil", "lease_until", "DATETIME(6)", reflect.TypeFor[*time.Time]()},
		{"LeaseVersion", "lease_version", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"NextAttemptAt", "next_attempt_at", "DATETIME(6)", reflect.TypeFor[*time.Time]()},
		{"AttemptCount", "attempt_count", "INT UNSIGNED", reflect.TypeFor[uint32]()},
		{"ExpiresAt", "expires_at", "DATETIME(6)", reflect.TypeFor[time.Time]()},
		{"LastErrorCode", "last_error_code", "VARCHAR(64)", reflect.TypeFor[*string]()},
		{"LastErrorMessage", "last_error_message", "VARCHAR(512)", reflect.TypeFor[*string]()},
		{"CreatedAt", "created_at", "DATETIME(6)", reflect.TypeFor[time.Time]()},
		{"UpdatedAt", "updated_at", "DATETIME(6)", reflect.TypeFor[time.Time]()},
		{"CompletedAt", "completed_at", "DATETIME(6)", reflect.TypeFor[*time.Time]()},
	}
	recordType := reflect.TypeFor[domain.UploadTask]()
	if len(columns) != 28 || len(fixtures) != 28 || recordType.NumField() != 28 {
		t.Fatalf("上传任务字段数量不符，数据库 %d，夹具 %d，领域 %d", len(columns), len(fixtures), recordType.NumField())
	}
	for _, fixture := range fixtures {
		t.Run(fixture.fieldName, func(t *testing.T) {
			// 2. 对照列类型和 NULL 约束，核对无符号范围、固定摘要及状态类型。
			field, exists := recordType.FieldByName(fixture.fieldName)
			column := columns[fixture.column]
			if !exists || field.Type != fixture.fieldType || len(column) != 4 || column[2] != fixture.sqlType {
				t.Fatal("上传任务字段或数据库类型不一致")
			}
			nullable := !strings.Contains(column[3], "NOT NULL")
			if nullable != (field.Type.Kind() == reflect.Pointer) {
				t.Fatal("上传任务字段的 NULL 语义不一致")
			}
			// 3. 可空字段的零值表示 NULL，非空指针可以保留明确的零值。
			if nullable {
				record := reflect.ValueOf(&domain.UploadTask{}).Elem()
				value := record.FieldByName(fixture.fieldName)
				if !value.IsNil() {
					t.Fatal("可空上传任务字段的默认值错误")
				}
				value.Set(reflect.New(field.Type.Elem()))
				if value.IsNil() || !value.Elem().IsZero() {
					t.Fatal("可空上传任务字段未保留明确零值")
				}
			}
		})
	}
}

func TestUploadTaskCreateInputFlow(t *testing.T) {
	// 1. 解析并校验客户端声明，取得有效展示名及独立摘要。
	for _, fixture := range []struct {
		name string
		body string
		size uint64
		hash *[sha256.Size]byte
	}{
		{"empty file without hash", `{"parent_id":"18446744073709551615","name":"  学习笔记.MD  ","size_bytes":0}`, 0, nil},
		{"single limit without hash", `{"parent_id":"18446744073709551615","name":"  学习笔记.MD  ","size_bytes":16777216,"sha256":null}`, 16777216, nil},
		{"explicit zero digest", `{"parent_id":"18446744073709551615","name":"  学习笔记.MD  ","size_bytes":1,"sha256":"` + strings.Repeat("00", 32) + `"}`, 1, new([sha256.Size]byte)},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var request dto.CreateUploadRequest
			if err := json.Unmarshal([]byte(fixture.body), &request); err != nil {
				t.Fatal(err)
			}
			input, fieldError := dto.ParseCreateUploadRequest(request)
			if fieldError != nil {
				t.Fatal(fieldError)
			}
			prepared, err := service.ValidateCreateUploadInput(*input)
			if err != nil {
				t.Fatal(err)
			}
			// 2. 结合可信身份与服务端暂存对象，组装 SINGLE 上传任务。
			actor := domain.Actor{UserID: 1 << 63, DeviceID: ^uint64(0), SessionID: 7}
			createdAt := time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC)
			task := domain.UploadTask{
				ID: ^uint64(0), UserID: actor.UserID, DeviceID: actor.DeviceID,
				StagedObjectID: 1 << 63, TargetParentID: prepared.ParentID,
				RequestedName: prepared.Name, UploadMode: "SINGLE",
				ExpectedSHA256: prepared.ExpectedSHA256, FileSize: prepared.SizeBytes,
				Status: domain.UploadStatusUploading, ReservedBytes: prepared.SizeBytes,
				ExpiresAt: createdAt.Add(24 * time.Hour), CreatedAt: createdAt, UpdatedAt: createdAt,
			}
			// 3. 核对字段衔接及创建阶段的可空结果。
			if task.ID != ^uint64(0) || task.UserID != actor.UserID || task.DeviceID != actor.DeviceID ||
				task.StagedObjectID != 1<<63 || task.TargetParentID != ^uint64(0) || task.RequestedName != "学习笔记.MD" ||
				task.UploadMode != "SINGLE" || task.FileSize != fixture.size || task.ReservedBytes != fixture.size ||
				!reflect.DeepEqual(task.ExpectedSHA256, fixture.hash) {
				t.Fatal("上传任务归属、规范化声明或预占字段衔接错误")
			}
			if !task.Status.IsValid() || task.Status.IsTerminal() || task.Status != domain.UploadStatusUploading ||
				task.PublishedObjectID != nil || task.ResolvedName != nil || task.ResultEntryID != nil || task.CompletedAt != nil ||
				task.PartSize != nil || task.PartCount != nil || task.StorageUploadID != nil ||
				task.LeaseOwner != nil || task.LeaseUntil != nil || task.LeaseVersion != 0 ||
				task.NextAttemptAt != nil || task.AttemptCount != 0 || task.LastErrorCode != nil || task.LastErrorMessage != nil {
				t.Fatal("上传任务初始状态或可空事实错误")
			}
			if task.CreatedAt != createdAt || task.UpdatedAt != createdAt || !task.ExpiresAt.Equal(createdAt.Add(24*time.Hour)) {
				t.Fatal("上传任务生命周期时间衔接错误")
			}
		})
	}
}
