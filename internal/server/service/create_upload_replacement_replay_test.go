package service_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func uploadReplacementReplayFixture(t *testing.T, size uint64) (*domain.IdempotencyRecord, service.UploadReplacementDeclaration) {
	t.Helper()
	created := time.Date(2020, 1, 1, 8, 30, 0, 123456000, time.UTC)
	declaration := service.UploadReplacementDeclaration{
		EntryID: 31, ExpectedRevision: 6, SizeBytes: size, ExpectedSHA256: sha256.Sum256([]byte("replacement replay fixture")),
	}
	task := domain.UploadTask{
		ID: 42, UserID: 7, DeviceID: 11, StagedObjectID: 12, TargetParentID: 13, RequestedName: "资料.bin",
		UploadMode: "SINGLE", Status: domain.UploadStatusUploading, FileSize: size, ReservedBytes: size,
		ExpectedSHA256: &declaration.ExpectedSHA256, ReplacementEntryID: &declaration.EntryID,
		ReplacementExpectedRevision: &declaration.ExpectedRevision,
		CreatedAt:                   created, UpdatedAt: created, ExpiresAt: created.Add(24 * time.Hour),
	}
	if size > domain.MaxSingleUploadSizeBytes {
		task.UploadMode, task.Status = "MULTIPART", domain.UploadStatusCreating
		// 历史计划使用固定的 32 MiB × 1600 片。
		task.PartSize, task.PartCount = new(uint64(32*1024*1024)), new(uint32(1600))
	}
	body, err := dto.EncodeCreateUploadReplacementSnapshot(&task)
	if err != nil {
		t.Fatal(err)
	}
	requestHash, err := service.BuildCreateUploadReplacementRequestHash(declaration.EntryID, declaration.ExpectedRevision, size, declaration.ExpectedSHA256)
	if err != nil {
		t.Fatal(err)
	}
	return &domain.IdempotencyRecord{
		ID: 23, UserID: task.UserID, Operation: domain.CreateUploadReplacementOperation, IdempotencyKey: "replace-31",
		RequestHash: requestHash, ResourceType: "UPLOAD_TASK", ResourceID: &task.ID, ResponseStatus: 201, ResponseBody: body,
		CreatedAt: created, ExpiresAt: created.Add(8 * 24 * time.Hour),
	}, declaration
}

func TestValidateCreateUploadReplacementReplaySuccess(t *testing.T) {
	for _, fixture := range []struct {
		name string
		size uint64
	}{
		{"zero byte SINGLE", 0},
		{"maximum SINGLE", domain.MaxSingleUploadSizeBytes},
		{"historical MULTIPART", 50 * 1024 * 1024 * 1024},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			record, declaration := uploadReplacementReplayFixture(t, fixture.size)
			original := bytes.Clone(record.ResponseBody)
			// 1. 上传任务超过 24 小时，幂等回执仍由自己的期限控制。
			for _, now := range []time.Time{record.CreatedAt.Add(25 * time.Hour), record.ExpiresAt.Add(-time.Nanosecond)} {
				err := service.ValidateCreateUploadReplacementReplay(record, 7, declaration, "replace-31", record.RequestHash, now,
					dto.ValidateCreateUploadReplacementSnapshot)
				if err != nil || !bytes.Equal(record.ResponseBody, original) {
					t.Fatalf("有效历史回执没有按原样通过，错误 %v", err)
				}
			}
		})
	}
}

func TestValidateCreateUploadReplacementReplayErrorOrder(t *testing.T) {
	record, declaration := uploadReplacementReplayFixture(t, 9)
	changed := declaration
	changed.ExpectedRevision++
	changedHash, err := service.BuildCreateUploadReplacementRequestHash(changed.EntryID, changed.ExpectedRevision, changed.SizeBytes, changed.ExpectedSHA256)
	if err != nil {
		t.Fatal(err)
	}
	check := func(userID uint64, checkTime time.Time, want error) {
		t.Helper()
		err := service.ValidateCreateUploadReplacementReplay(record, userID, changed, "replace-31", changedHash, checkTime,
			func([]byte, uint64, uint64, uint64, uint64) error {
				t.Fatal("回执范围、期限或摘要失败后仍解码快照")
				return nil
			})
		if !errors.Is(err, want) {
			t.Fatalf("拒绝顺序错误，得到 %v，期望 %v", err, want)
		}
	}
	// 1. 范围先于到期，到期先于摘要，摘要冲突先于快照内容。
	record.ResponseBody = nil
	check(8, record.ExpiresAt, domain.ErrInvalidIdempotencyRecordData)
	check(7, record.ExpiresAt, domain.ErrIdempotencyRecordExpired)
	check(7, record.ExpiresAt.Add(-time.Nanosecond), domain.ErrIdempotencyRequestConflict)
}

func TestValidateCreateUploadReplacementReplayRejectsInvalidReceipt(t *testing.T) {
	otherMode, _ := uploadReplacementReplayFixture(t, 50*1024*1024*1024)
	replaceBody := func(record *domain.IdempotencyRecord, old, replacement string) {
		record.ResponseBody = json.RawMessage(strings.Replace(string(record.ResponseBody), old, replacement, 1))
	}
	for _, fixture := range []struct {
		name   string
		change func(*domain.IdempotencyRecord)
	}{
		{"missing record ID", func(record *domain.IdempotencyRecord) { record.ID = 0 }},
		{"ordinary upload scope", func(record *domain.IdempotencyRecord) { record.Operation = domain.CreateUploadOperation }},
		{"wrong key case", func(record *domain.IdempotencyRecord) { record.IdempotencyKey = "Replace-31" }},
		{"malformed retention interval", func(record *domain.IdempotencyRecord) { record.ExpiresAt = record.CreatedAt }},
		{"wrong resource type", func(record *domain.IdempotencyRecord) { record.ResourceType = "ENTRY" }},
		{"missing task resource", func(record *domain.IdempotencyRecord) { record.ResourceID = nil }},
		{"zero task resource", func(record *domain.IdempotencyRecord) { *record.ResourceID = 0 }},
		{"wrong task resource", func(record *domain.IdempotencyRecord) { *record.ResourceID = 43 }},
		{"wrong response status", func(record *domain.IdempotencyRecord) { record.ResponseStatus = 202 }},
		{"empty response body", func(record *domain.IdempotencyRecord) { record.ResponseBody = nil }},
		{"wrong target", func(record *domain.IdempotencyRecord) { replaceBody(record, `"entry_id":"31"`, `"entry_id":"32"`) }},
		{"wrong expected revision", func(record *domain.IdempotencyRecord) {
			replaceBody(record, `"expected_revision":"6"`, `"expected_revision":"7"`)
		}},
		{"wrong size", func(record *domain.IdempotencyRecord) { replaceBody(record, `"size_bytes":9`, `"size_bytes":0`) }},
		{"wrong upload mode", func(record *domain.IdempotencyRecord) { record.ResponseBody = otherMode.ResponseBody }},
		{"non-initial result", func(record *domain.IdempotencyRecord) {
			replaceBody(record, `"published_revision":null`, `"published_revision":"7"`)
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			record, declaration := uploadReplacementReplayFixture(t, 9)
			fixture.change(record)
			err := service.ValidateCreateUploadReplacementReplay(record, 7, declaration, "replace-31", record.RequestHash,
				record.CreatedAt.Add(time.Hour), dto.ValidateCreateUploadReplacementSnapshot)
			if !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatalf("异常回执应返回固定数据错误，得到 %v", err)
			}
		})
	}
}
