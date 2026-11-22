package dto

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const replacementSnapshotBinding = `"replacement":{"entry_id":"18446744073709551615","expected_revision":"18446744073709551614","published_revision":null,"conflicting_revision":null}`

func uploadReplacementSnapshotTask(mode string) domain.UploadTask {
	task := uploadTaskDataFixture()
	task.LeaseOwner, task.LeaseUntil, task.LastErrorMessage = nil, nil, nil
	task.LeaseVersion, task.AttemptCount = 0, 0
	if mode == "MULTIPART" {
		task = multipartCreationTask()
	}
	task.ReplacementEntryID = new(uint64(math.MaxUint64))
	task.ReplacementExpectedRevision = new(uint64(math.MaxUint64 - 1))
	return task
}

func TestCreateUploadReplacementSnapshotRoundTrip(t *testing.T) {
	singleBody, _ := savedCreateUploadFixture(t)
	for _, fixture := range []struct{ mode, ordinaryBody string }{
		{"SINGLE", string(singleBody)},
		{"MULTIPART", multipartCreationSnapshot},
	} {
		t.Run(fixture.mode, func(t *testing.T) {
			// 1. 核对公开 JSON 仅在普通模式的数据上增加原绑定。
			task := uploadReplacementSnapshotTask(fixture.mode)
			body, err := EncodeCreateUploadReplacementSnapshot(&task)
			want := strings.Replace(fixture.ordinaryBody, `"last_error":null}`, `"last_error":null,`+replacementSnapshotBinding+"}", 1)
			if err != nil || string(body) != want {
				t.Fatalf("公开替换快照不符，得到 %s，错误 %v", body, err)
			}
			if err := ValidateCreateUploadSnapshot(body); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatalf("普通恢复入口接受了替换快照，错误 %v", err)
			}
			var reordered map[string]json.RawMessage
			if err := json.Unmarshal(body, &reordered); err != nil {
				t.Fatal(err)
			}
			formatted, err := json.MarshalIndent(reordered, "", "  ")
			if err != nil {
				t.Fatal(err)
			}

			// 2. 恢复紧凑及重排 JSON，保持首次的路径、计划和截止时间。
			for _, encoded := range [][]byte{body, formatted} {
				var restored any
				if fixture.mode == "SINGLE" {
					data, err := RestoreCreateUploadReplacementData(encoded)
					if err != nil || data.ContentPath != "/api/v1/uploads/19/content" ||
						data.Task.Replacement == nil || data.Task.Replacement.ExpectedRevision != "18446744073709551614" {
						t.Fatalf("恢复 SINGLE 快照失败 %+v，错误 %v", data, err)
					}
					restored = data
				} else {
					data, err := RestoreCreateMultipartUploadReplacementData(encoded)
					if err != nil || data.PartSizeBytes != 16777216 || data.PartCount != 2 ||
						data.Task.Replacement == nil || data.Task.Replacement.EntryID != "18446744073709551615" {
						t.Fatalf("恢复 MULTIPART 快照失败 %+v，错误 %v", data, err)
					}
					restored = data
				}
				roundTrip, err := json.Marshal(restored)
				if err != nil || string(roundTrip) != want {
					t.Fatalf("恢复后公开快照变化，得到 %s，错误 %v", roundTrip, err)
				}
			}
		})
	}
}

func TestRestoreCreateUploadReplacementSnapshotPreservesHistoricalLayout(t *testing.T) {
	task := uploadReplacementSnapshotTask("MULTIPART")
	task.FileSize, task.ReservedBytes = 50*1024*1024*1024, 50*1024*1024*1024
	*task.PartSize, *task.PartCount = 32*1024*1024, 1600
	task.CreatedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	task.UpdatedAt, task.ExpiresAt = task.CreatedAt, task.CreatedAt.Add(24*time.Hour)
	body, err := EncodeCreateUploadReplacementSnapshot(&task)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := RestoreCreateMultipartUploadReplacementData(body)
	if err != nil || saved.PartSizeBytes != 32*1024*1024 || saved.PartCount != 1600 || !saved.ExpiresAt.Equal(task.ExpiresAt) {
		t.Fatalf("首次片长、片数或截止时间未保留 %+v，错误 %v", saved, err)
	}
	saved.Task.Replacement.EntryID = "1"
	fresh, err := RestoreCreateMultipartUploadReplacementData(body)
	if err != nil || fresh.Task.Replacement.EntryID != "18446744073709551615" || reflect.DeepEqual(saved, fresh) {
		t.Fatalf("一次恢复后的修改影响了原快照 %+v，错误 %v", fresh, err)
	}
}

func TestEncodeCreateUploadReplacementSnapshotRejectsInvalidTask(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		change func(*domain.UploadTask)
	}{
		{"ordinary task", func(task *domain.UploadTask) { task.ReplacementEntryID, task.ReplacementExpectedRevision = nil, nil }},
		{"incomplete binding", func(task *domain.UploadTask) { task.ReplacementExpectedRevision = nil }},
		{"missing digest", func(task *domain.UploadTask) { task.ExpectedSHA256 = nil }},
		{"early published revision", func(task *domain.UploadTask) { task.ReplacementPublishedRevision = new(uint64(math.MaxUint64)) }},
		{"early conflicting revision", func(task *domain.UploadTask) { task.ReplacementConflictingRevision = new(uint64(7)) }},
		{"verification already started", func(task *domain.UploadTask) { task.Status = domain.UploadStatusVerifying }},
		{"lease already used", func(task *domain.UploadTask) { task.LeaseVersion = 1 }},
		{"single with part count", func(task *domain.UploadTask) { task.PartCount = new(uint32(2)) }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			task := uploadReplacementSnapshotTask("SINGLE")
			fixture.change(&task)
			body, err := EncodeCreateUploadReplacementSnapshot(&task)
			if body != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
				t.Fatalf("异常初始任务得到快照 %s，错误 %v", body, err)
			}
		})
	}
}

func TestRestoreCreateUploadReplacementSnapshotRejectsInvalidData(t *testing.T) {
	for _, mode := range []string{"SINGLE", "MULTIPART"} {
		t.Run(mode, func(t *testing.T) {
			task := uploadReplacementSnapshotTask(mode)
			body, err := EncodeCreateUploadReplacementSnapshot(&task)
			if err != nil {
				t.Fatal(err)
			}
			for _, fixture := range []struct{ name, from, to string }{
				{"missing replacement", "," + replacementSnapshotBinding, ""},
				{"null replacement", replacementSnapshotBinding, `"replacement":null`},
				{"missing result even if nil", `,"conflicting_revision":null`, ""},
				{"published result", `"published_revision":null`, `"published_revision":"7"`},
				{"conflicting result", `"conflicting_revision":null`, `"conflicting_revision":"7"`},
				{"numeric revision", `"expected_revision":"18446744073709551614"`, `"expected_revision":6`},
				{"overflow revision", `"expected_revision":"18446744073709551614"`, `"expected_revision":"18446744073709551616"`},
				{"zero target", `"entry_id":"18446744073709551615"`, `"entry_id":"0"`},
				{"padded target", `"entry_id":"18446744073709551615"`, `"entry_id":"031"`},
				{"private field in binding", `"conflicting_revision":null`, `"conflicting_revision":null,"source_device_id":"7"`},
				{"duplicate in binding", `"published_revision":null`, `"published_revision":null,"published\u005frevision":null`},
				{"duplicate in task", `"last_error":null`, `"last_error":null,"last_error":null`},
				{"duplicate in data", `},"upload_mode":`, `},"upload_mode":` + `"` + mode + `","upload_mode":`},
				{"unexpected request ID", `{"task":`, `{"request_id":"synthetic","task":`},
				{"initial task result", `"result_entry_id":null`, `"result_entry_id":"7"`},
			} {
				t.Run(fixture.name, func(t *testing.T) {
					changed := strings.Replace(string(body), fixture.from, fixture.to, 1)
					if changed == string(body) {
						t.Fatal("夹具未修改")
					}
					if mode == "SINGLE" {
						if data, err := RestoreCreateUploadReplacementData([]byte(changed)); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
							t.Fatalf("接受异常 SINGLE 快照 %+v，错误 %v", data, err)
						}
					} else if data, err := RestoreCreateMultipartUploadReplacementData([]byte(changed)); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
						t.Fatalf("接受异常 MULTIPART 快照 %+v，错误 %v", data, err)
					}
				})
			}
			if mode == "SINGLE" {
				changed := strings.Replace(string(body), "/uploads/19/content", "/uploads/20/content", 1)
				if data, err := RestoreCreateUploadReplacementData([]byte(changed)); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
					t.Fatalf("接受其他任务的内容路径 %+v，错误 %v", data, err)
				}
			} else {
				changed := strings.Replace(string(body), `"part_count":2`, `"part_count":1`, 1)
				if data, err := RestoreCreateMultipartUploadReplacementData([]byte(changed)); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
					t.Fatalf("接受错误的分片计划 %+v，错误 %v", data, err)
				}
			}
		})
	}
}
