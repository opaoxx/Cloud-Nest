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

func TestStoredObjectRecordShape(t *testing.T) {
	// 1. 从正式迁移提取对象列，准备领域字段与准确类型。
	_, table, found := strings.Cut(migrations.InitialSQL, "CREATE TABLE stored_objects (\n")
	if !found {
		t.Fatal("正式迁移缺少存储对象表")
	}
	table, _, found = strings.Cut(table, ") ENGINE=")
	if !found {
		t.Fatal("存储对象表定义不完整")
	}
	columnPattern := regexp.MustCompile(`(?m)^ ([a-z_][a-z_0-9]*) (BIGINT UNSIGNED|VARCHAR\(\d+\)|BINARY\(32\)|DATETIME\(6\))([^\n]*)`)
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
		{"OwnerUserID", "owner_user_id", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"Provider", "provider", "VARCHAR(32)", reflect.TypeFor[string]()},
		{"Bucket", "bucket", "VARCHAR(63)", reflect.TypeFor[string]()},
		{"ObjectKey", "object_key", "VARCHAR(255)", reflect.TypeFor[string]()},
		{"StorageVersionID", "storage_version_id", "VARCHAR(255)", reflect.TypeFor[*string]()},
		{"FileSize", "file_size", "BIGINT UNSIGNED", reflect.TypeFor[uint64]()},
		{"VerifiedSHA256", "verified_sha256", "BINARY(32)", reflect.TypeFor[*[sha256.Size]byte]()},
		{"Status", "status", "VARCHAR(16)", reflect.TypeFor[domain.StoredObjectStatus]()},
		{"DedupHash", "dedup_hash", "BINARY(32)", reflect.TypeFor[*[sha256.Size]byte]()},
		{"StorageETag", "storage_etag", "VARCHAR(255)", reflect.TypeFor[*string]()},
		{"VerifiedAt", "verified_at", "DATETIME(6)", reflect.TypeFor[*time.Time]()},
		{"UnreferencedAt", "unreferenced_at", "DATETIME(6)", reflect.TypeFor[*time.Time]()},
		{"GCAfter", "gc_after", "DATETIME(6)", reflect.TypeFor[*time.Time]()},
		{"LastErrorCode", "last_error_code", "VARCHAR(64)", reflect.TypeFor[*string]()},
		{"CreatedAt", "created_at", "DATETIME(6)", reflect.TypeFor[time.Time]()},
		{"UpdatedAt", "updated_at", "DATETIME(6)", reflect.TypeFor[time.Time]()},
	}
	recordType := reflect.TypeFor[domain.StoredObject]()
	if len(columns) != 17 || len(fixtures) != 17 || recordType.NumField() != 17 {
		t.Fatalf("存储对象字段数量不符，数据库 %d，夹具 %d，领域 %d", len(columns), len(fixtures), recordType.NumField())
	}
	for _, fixture := range fixtures {
		t.Run(fixture.fieldName, func(t *testing.T) {
			// 2. 对照类型、空值与生成列事实，核对内部记录表示。
			field, exists := recordType.FieldByName(fixture.fieldName)
			column := columns[fixture.column]
			if !exists || field.Type != fixture.fieldType || len(column) != 4 || column[2] != fixture.sqlType || field.Tag != "" {
				t.Fatal("存储对象字段、类型或领域标签约定不一致")
			}
			nullable := !strings.Contains(column[3], "NOT NULL")
			if nullable != (field.Type.Kind() == reflect.Pointer) {
				t.Fatal("存储对象字段的 NULL 语义不一致")
			}
			generated := strings.Contains(column[3], "GENERATED ALWAYS AS")
			if generated != (fixture.fieldName == "DedupHash") {
				t.Fatal("对象去重生成列的记录位置不一致")
			}
			// 3. 可空字段用 nil 表达 NULL，非空指针保留明确零值。
			if nullable {
				record := reflect.ValueOf(&domain.StoredObject{}).Elem()
				value := record.FieldByName(fixture.fieldName)
				if !value.IsNil() {
					t.Fatal("可空对象字段的默认值错误")
				}
				value.Set(reflect.New(field.Type.Elem()))
				if value.IsNil() || !value.Elem().IsZero() {
					t.Fatal("可空对象字段未保留明确零值")
				}
			}
		})
	}
}

func TestStoredObjectUploadPreparationFlow(t *testing.T) {
	for _, fixture := range []struct {
		name string
		body string
		size uint64
	}{
		{"empty file", `{"parent_id":"13","name":"  学习笔记.MD  ","size_bytes":0}`, 0},
		{"single limit", `{"parent_id":"13","name":"  学习笔记.MD  ","size_bytes":16777216,"sha256":null}`, 16777216},
		{"declared digest", `{"parent_id":"13","name":"  学习笔记.MD  ","size_bytes":1,"sha256":"` + strings.Repeat("ab", sha256.Size) + `"}`, 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 通过现有 JSON、DTO 和业务校验取得上传声明。
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
			// 2. 用可信身份、服务端位置与统一时间组装暂存对象及任务。
			actor := domain.Actor{UserID: ^uint64(0), DeviceID: 11, SessionID: 17}
			createdAt := time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC)
			object := domain.StoredObject{
				ID: ^uint64(0), OwnerUserID: actor.UserID,
				Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "uploads/test-attempt/object",
				FileSize: prepared.SizeBytes, Status: domain.StoredObjectStatusStaging,
				CreatedAt: createdAt, UpdatedAt: createdAt,
			}
			task := domain.UploadTask{
				ID: 19, UserID: actor.UserID, DeviceID: actor.DeviceID, StagedObjectID: object.ID,
				TargetParentID: prepared.ParentID, RequestedName: prepared.Name, UploadMode: "SINGLE",
				ExpectedSHA256: prepared.ExpectedSHA256, FileSize: prepared.SizeBytes,
				Status: domain.UploadStatusUploading, ReservedBytes: prepared.SizeBytes,
				ExpiresAt: createdAt.Add(24 * time.Hour), CreatedAt: createdAt, UpdatedAt: createdAt,
			}
			// 3. 核对归属、大小和暂存引用，保留尚未验证的空值事实。
			if object.ID != ^uint64(0) || object.OwnerUserID != actor.UserID ||
				object.Provider != "local_minio" || object.Bucket != "cloudnest" || object.ObjectKey != "uploads/test-attempt/object" ||
				object.FileSize != fixture.size || object.Status != domain.StoredObjectStatusStaging || !object.Status.IsValid() ||
				object.CreatedAt != createdAt || object.UpdatedAt != createdAt {
				t.Fatal("暂存对象归属、位置、大小或初始状态错误")
			}
			if object.StorageVersionID != nil || object.VerifiedSHA256 != nil || object.DedupHash != nil ||
				object.StorageETag != nil || object.VerifiedAt != nil || object.UnreferencedAt != nil ||
				object.GCAfter != nil || object.LastErrorCode != nil {
				t.Fatal("暂存对象可空事实的初始值错误")
			}
			if task.StagedObjectID != object.ID || task.UserID != object.OwnerUserID || task.FileSize != object.FileSize ||
				task.ReservedBytes != fixture.size || task.TargetParentID != 13 || task.RequestedName != "学习笔记.MD" ||
				task.Status != domain.UploadStatusUploading || !reflect.DeepEqual(task.ExpectedSHA256, prepared.ExpectedSHA256) {
				t.Fatal("暂存对象与上传任务声明衔接错误")
			}
			if (fixture.name == "declared digest") != (task.ExpectedSHA256 != nil) {
				t.Fatal("客户端声明摘要未保留在上传任务中")
			}
		})
	}
}
