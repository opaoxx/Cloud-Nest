package dto

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const multipartCreationSnapshot = `{"task":{"id":"20","parent_id":"11","requested_name":"合成分片.bin","resolved_name":null,"size_bytes":16777217,"upload_mode":"MULTIPART","status":"CREATING","result_entry_id":null,"created_at":"2026-11-21T08:00:00.123456Z","updated_at":"2026-11-21T08:00:00.123456Z","expires_at":"2026-11-22T08:00:00.123456Z","completed_at":null,"last_error":null},"upload_mode":"MULTIPART","part_size_bytes":16777216,"part_count":2,"expires_at":"2026-11-22T08:00:00.123456Z"}`

func multipartCreationTask() domain.UploadTask {
	digest := sha256.Sum256([]byte("synthetic"))
	createdAt := time.Date(2026, 11, 21, 16, 0, 0, 123456789, time.FixedZone("fixture", 8*3600))
	return domain.UploadTask{
		ID: 20, UserID: 7, DeviceID: 11, StagedObjectID: 23, TargetParentID: 11,
		RequestedName: "合成分片.bin", UploadMode: "MULTIPART", Status: domain.UploadStatusCreating,
		FileSize: 16777217, ReservedBytes: 16777217, ExpectedSHA256: &digest,
		PartSize: new(uint64(16777216)), PartCount: new(uint32(2)),
		CreatedAt: createdAt, UpdatedAt: createdAt, ExpiresAt: createdAt.Add(24 * time.Hour),
	}
}

func TestCreateMultipartUploadDataRoundTrip(t *testing.T) {
	// 1. 确认编码与 OpenAPI 示例一致，并且不交付私有字段。
	input := multipartCreationTask()
	data, err := NewCreateMultipartUploadData(&input)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(data)
	if err != nil || string(body) != multipartCreationSnapshot {
		t.Fatalf("公开创建快照不符: %s %v", body, err)
	}
	*input.PartSize, *input.PartCount = 0, 0

	// 2. 用紧凑 JSON 和字段重排后的 JSON 恢复，保持相同的计划和时间。
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	formatted, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for _, encoded := range [][]byte{body, formatted} {
		restored, err := RestoreCreateMultipartUploadData(encoded)
		if err != nil || !reflect.DeepEqual(restored, data) || restored.ExpiresAt.Location() != time.UTC {
			t.Fatalf("未正确恢复固定快照: %+v %v", restored, err)
		}
	}
}

func TestRestoreCreateMultipartUploadDataPreservesSavedLayout(t *testing.T) {
	// 任务已过期时也恢复原快照，并按保存的片长核对片数。
	input := multipartCreationTask()
	input.FileSize, input.ReservedBytes = 50*1024*1024*1024, 50*1024*1024*1024
	*input.PartSize, *input.PartCount = 32*1024*1024, 1600
	input.CreatedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	input.UpdatedAt, input.ExpiresAt = input.CreatedAt, input.CreatedAt.Add(24*time.Hour)
	data, err := NewCreateMultipartUploadData(&input)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreCreateMultipartUploadData(body)
	if err != nil || !reflect.DeepEqual(restored, data) ||
		restored.PartSizeBytes != 32*1024*1024 || restored.PartCount != 1600 || !restored.ExpiresAt.Equal(input.ExpiresAt) {
		t.Fatalf("历史快照中的计划或时间被重算: %+v %v", restored, err)
	}
}

func TestNewCreateMultipartUploadDataRejectsInvalidTask(t *testing.T) {
	if data, err := NewCreateMultipartUploadData(nil); data != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
		t.Fatalf("接受空任务: %+v %v", data, err)
	}
	for _, fixture := range []struct {
		name   string
		change func(*domain.UploadTask)
	}{
		{"mode", func(task *domain.UploadTask) { task.UploadMode = "SINGLE" }},
		{"status", func(task *domain.UploadTask) { task.Status = domain.UploadStatusUploading }},
		{"missing digest", func(task *domain.UploadTask) { task.ExpectedSHA256 = nil }},
		{"partial reservation", func(task *domain.UploadTask) { task.ReservedBytes-- }},
		{"storage session attached", func(task *domain.UploadTask) { task.StorageUploadID = new("fixture") }},
		{"internal diagnostic", func(task *domain.UploadTask) { task.LastErrorMessage = new("fixture") }},
		{"missing part size", func(task *domain.UploadTask) { task.PartSize = nil }},
		{"missing part count", func(task *domain.UploadTask) { task.PartCount = nil }},
		{"wrong part count", func(task *domain.UploadTask) { *task.PartCount = 3 }},
		{"zero expiry", func(task *domain.UploadTask) { task.ExpiresAt = time.Time{} }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			task := multipartCreationTask()
			fixture.change(&task)
			if data, err := NewCreateMultipartUploadData(&task); data != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
				t.Fatalf("接受异常初始任务: %+v %v", data, err)
			}
		})
	}
}

func TestRestoreCreateMultipartUploadDataRejectsInvalidSnapshot(t *testing.T) {
	for _, fixture := range []struct{ name, from, to string }{
		{"missing part size", `"part_size_bytes":16777216,`, ""},
		{"null part count", `"part_count":2`, `"part_count":null`},
		{"unknown top field", `"part_count":2`, `"part_count":2,"content_path":"/fixture"`},
		{"private task field", `"last_error":null`, `"last_error":null,"storage_upload_id":"fixture"`},
		{"missing initial null", `,"last_error":null`, ""},
		{"non-null initial result", `"result_entry_id":null`, `"result_entry_id":"42"`},
		{"task id", `"id":"20"`, `"id":"020"`},
		{"mode mismatch", `"upload_mode":"MULTIPART"`, `"upload_mode":"SINGLE"`},
		{"advanced status", `"status":"CREATING"`, `"status":"UPLOADING"`},
		{"single sized file", `"size_bytes":16777217`, `"size_bytes":16777216`},
		{"unsafe file size", `"size_bytes":16777217`, `"size_bytes":9007199254740992`},
		{"unsafe part size", `"part_size_bytes":16777216`, `"part_size_bytes":9007199254740992`},
		{"small parts", `"part_size_bytes":16777216`, `"part_size_bytes":8388608`},
		{"misaligned part size", `"part_size_bytes":16777216`, `"part_size_bytes":16777217`},
		{"wrong part count", `"part_count":2`, `"part_count":1`},
		{"too many parts", `"part_count":2`, `"part_count":10001`},
		{"non-integer part count", `"part_count":2`, `"part_count":2.5`},
		{"different deadline", `"expires_at":"2026-11-22T08:00:00.123456Z"`, `"expires_at":"2026-11-22T08:00:00.123457Z"`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			changed := strings.Replace(multipartCreationSnapshot, fixture.from, fixture.to, 1)
			if changed == multipartCreationSnapshot {
				t.Fatal("夹具未修改")
			}
			if data, err := RestoreCreateMultipartUploadData([]byte(changed)); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatalf("接受异常创建快照: %+v %v", data, err)
			}
		})
	}
	single, _ := savedCreateUploadFixture(t)
	if data, err := RestoreCreateMultipartUploadData(single); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
		t.Fatal("SINGLE 快照被 Multipart 恢复入口接受", err)
	}
}
