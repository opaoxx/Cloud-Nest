package service

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func createUploadReplayFixture(t *testing.T) (*domain.IdempotencyRecord, time.Time) {
	t.Helper()
	hash, err := BuildCreateUploadRequestHash(domain.CreateUploadInput{ParentID: 11, Name: "学习资料", SizeBytes: 2048})
	if err != nil {
		t.Fatal(err)
	}
	resourceID := uint64(42)
	createdAt := time.Date(2026, 11, 16, 1, 30, 0, 123456000, time.UTC)
	return &domain.IdempotencyRecord{
		ID: 23, UserID: 7, Operation: domain.CreateUploadOperation, IdempotencyKey: "request-007", RequestHash: hash,
		ResourceType: "UPLOAD_TASK", ResourceID: &resourceID, ResponseStatus: 201,
		ResponseBody: json.RawMessage(`{"task":{"id":"42","parent_id":"11","requested_name":"学习资料","resolved_name":null,"size_bytes":2048,"upload_mode":"SINGLE","status":"UPLOADING","result_entry_id":null,"created_at":"2026-11-16T01:30:00.123456Z","updated_at":"2026-11-16T01:30:00.123456Z","expires_at":"2026-11-17T01:30:00.123456Z","completed_at":null,"last_error":null},"upload_mode":"SINGLE","content_path":"/api/v1/uploads/42/content","expires_at":"2026-11-17T01:30:00.123456Z"}`),
		CreatedAt:    createdAt, ExpiresAt: createdAt.Add(8 * 24 * time.Hour),
	}, createdAt.Add(time.Hour)
}

func TestValidateCreateUploadReplaySuccess(t *testing.T) {
	for _, name := range []string{"ordinary", "deadline minus nanosecond", "timezone", "maximum ids", "minimum key", "maximum key", "date bounds", "reformatted json", "task already expired"} {
		t.Run(name, func(t *testing.T) {
			record, now := createUploadReplayFixture(t)
			switch name {
			case "deadline minus nanosecond":
				now = record.ExpiresAt.Add(-time.Nanosecond)
			case "task already expired":
				now = record.CreatedAt.Add(25 * time.Hour)
			case "timezone":
				zone := time.FixedZone("UTC+8", 8*3600)
				record.CreatedAt, record.ExpiresAt, now = record.CreatedAt.In(zone), record.ExpiresAt.In(zone), now.In(zone)
			case "maximum ids":
				record.ID, record.UserID, *record.ResourceID = ^uint64(0), ^uint64(0), ^uint64(0)
				record.ResponseBody = json.RawMessage(strings.Replace(string(record.ResponseBody), `"id":"42"`, `"id":"18446744073709551615"`, 1))
				record.ResponseBody = json.RawMessage(strings.Replace(string(record.ResponseBody), "/api/v1/uploads/42/content", "/api/v1/uploads/18446744073709551615/content", 1))
			case "minimum key":
				record.IdempotencyKey = "!"
			case "maximum key":
				record.IdempotencyKey = strings.Repeat("~", 128)
			case "date bounds":
				record.CreatedAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				record.ExpiresAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
			case "reformatted json":
				var payload map[string]any
				if err := json.Unmarshal(record.ResponseBody, &payload); err != nil {
					t.Fatal(err)
				}
				encoded, err := json.MarshalIndent(payload, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				record.ResponseBody = encoded
			}
			if err := ValidateCreateUploadReplay(record, record.UserID, record.IdempotencyKey, record.RequestHash, now); err != nil {
				t.Fatal("合法同键同请求未通过", err)
			}
		})
	}
}

func TestValidateCreateUploadReplayInvalidRecords(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		change func(*domain.IdempotencyRecord)
	}{
		{"zero record id", func(record *domain.IdempotencyRecord) { record.ID = 0 }},
		{"other user", func(record *domain.IdempotencyRecord) { record.UserID = 8 }},
		{"other operation", func(record *domain.IdempotencyRecord) { record.Operation = "v2.uploads.create" }},
		{"other key", func(record *domain.IdempotencyRecord) { record.IdempotencyKey = "another-key" }},
		{"key case", func(record *domain.IdempotencyRecord) { record.IdempotencyKey = "REQUEST-007" }},
		{"zero creation time", func(record *domain.IdempotencyRecord) { record.CreatedAt = time.Time{} }},
		{"zero expiration", func(record *domain.IdempotencyRecord) { record.ExpiresAt = time.Time{} }},
		{"expiration equals creation", func(record *domain.IdempotencyRecord) { record.ExpiresAt = record.CreatedAt }},
		{"expiration precedes creation", func(record *domain.IdempotencyRecord) { record.ExpiresAt = record.CreatedAt.Add(-time.Microsecond) }},
		{"creation before database range", func(record *domain.IdempotencyRecord) {
			record.CreatedAt = time.Date(999, 12, 31, 23, 59, 59, 0, time.UTC)
		}},
		{"expiration after database range", func(record *domain.IdempotencyRecord) {
			record.ExpiresAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		}},
		{"utc expiration overflow", func(record *domain.IdempotencyRecord) {
			record.ExpiresAt = time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600))
		}},
		{"other resource type", func(record *domain.IdempotencyRecord) { record.ResourceType = "ENTRY" }},
		{"missing resource", func(record *domain.IdempotencyRecord) { record.ResourceID = nil }},
		{"zero resource", func(record *domain.IdempotencyRecord) { *record.ResourceID = 0 }},
		{"other status", func(record *domain.IdempotencyRecord) { record.ResponseStatus = 200 }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			record, now := createUploadReplayFixture(t)
			testCase.change(record)
			if err := ValidateCreateUploadReplay(record, 7, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || err.Error() != "幂等记录数据异常" {
				t.Fatal("异常记录未返回固定数据错误", err)
			}
		})
	}
	record, now := createUploadReplayFixture(t)
	if err := ValidateCreateUploadReplay(nil, 7, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
		t.Fatal("成功读取后的空记录未拒绝", err)
	}
	for _, body := range []string{
		"", "null", "[]", "private-json", `{"task":null}`, `{"task":{}}`, `{"task":[]}`,
		`{"task":{"id":42}}`, `{"task":{"id":"0"}}`, `{"task":{"id":"042"}}`,
		`{"task":{"id":"43"}}`, `{"task":{"id":"18446744073709551616"}}`,
		`{"task":{"id":"42"}} {}`, "{\"task\":{\"id\":\"42\",\"name\":\"private\xff\"}}",
	} {
		record.ResponseBody = json.RawMessage(body)
		if err := ValidateCreateUploadReplay(record, 7, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
			t.Fatal("正文语法或资源标识错误未拒绝", err)
		}
	}
	record.ResponseBody = nil
	if err := ValidateCreateUploadReplay(record, 7, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
		t.Fatal("SQL NULL 正文未拒绝", err)
	}
}

func TestValidateCreateUploadReplayInputs(t *testing.T) {
	record, now := createUploadReplayFixture(t)
	for _, key := range []string{"", " ", "a,b", "a b", "key\t", "中文", strings.Repeat("a", 129)} {
		if err := ValidateCreateUploadReplay(record, 7, key, record.RequestHash, now); err == nil || errors.Is(err, domain.ErrIdempotencyRequestConflict) || errors.Is(err, domain.ErrIdempotencyRecordExpired) {
			t.Fatal("无效调用键未作为调用错误处理", err)
		}
	}
	if err := ValidateCreateUploadReplay(record, 0, record.IdempotencyKey, record.RequestHash, now); err == nil {
		t.Fatal("零用户未拒绝")
	}
	if err := ValidateCreateUploadReplay(record, 7, record.IdempotencyKey, record.RequestHash, time.Time{}); err == nil {
		t.Fatal("零判断时间未拒绝")
	}
}

func TestValidateCreateUploadReplayConflictAndExpiry(t *testing.T) {
	record, now := createUploadReplayFixture(t)
	zeroDigest, otherDigest := [32]byte{}, [32]byte{1}
	for _, input := range []domain.CreateUploadInput{
		{ParentID: 11, Name: "学习资料", SizeBytes: 2048},
		{ParentID: 11, Name: "  学习资料\u3000", SizeBytes: 2048},
		{ParentID: 11, Name: "其他资料", SizeBytes: 2048},
		{ParentID: 12, Name: "学习资料", SizeBytes: 2048},
		{ParentID: 11, Name: "学习资料", SizeBytes: 2049},
		{ParentID: 11, Name: "学习资料", SizeBytes: 0},
		{ParentID: 11, Name: "学习资料", SizeBytes: 2048, ExpectedSHA256: &zeroDigest},
		{ParentID: 11, Name: "学习资料", SizeBytes: 2048, ExpectedSHA256: &otherDigest},
	} {
		hash, err := BuildCreateUploadRequestHash(input)
		if err != nil {
			t.Fatal(err)
		}
		err = ValidateCreateUploadReplay(record, 7, record.IdempotencyKey, hash, now)
		if hash == record.RequestHash {
			if err != nil {
				t.Fatal("规范化后的相同请求未复用", err)
			}
		} else if !errors.Is(err, domain.ErrIdempotencyRequestConflict) || err.Error() != "幂等键已用于不同请求参数" {
			t.Fatal("参数差异未返回固定冲突错误", err)
		}
	}

	for _, checkTime := range []time.Time{record.ExpiresAt, record.ExpiresAt.Add(time.Nanosecond), record.ExpiresAt.Add(24 * time.Hour)} {
		for _, sameHash := range []bool{true, false} {
			hash := record.RequestHash
			if !sameHash {
				hash[0] ^= 0xff
			}
			if err := ValidateCreateUploadReplay(record, 7, record.IdempotencyKey, hash, checkTime); !errors.Is(err, domain.ErrIdempotencyRecordExpired) {
				t.Fatal("到期判断或到期优先顺序错误", err)
			}
		}
	}
	// 范围异常先于到期，摘要冲突先于具体结果读取。
	record.UserID = 8
	if err := ValidateCreateUploadReplay(record, 7, record.IdempotencyKey, record.RequestHash, record.ExpiresAt); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
		t.Fatal("范围异常被误判为到期", err)
	}
	record.UserID, record.ResponseBody = 7, nil
	hash := record.RequestHash
	hash[31] ^= 0xff
	if err := ValidateCreateUploadReplay(record, 7, record.IdempotencyKey, hash, now); !errors.Is(err, domain.ErrIdempotencyRequestConflict) {
		t.Fatal("摘要冲突判断顺序错误", err)
	}
}

func TestValidateCreateUploadReplayIndependent(t *testing.T) {
	record, now := createUploadReplayFixture(t)
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var original domain.IdempotencyRecord
	if err := json.Unmarshal(encoded, &original); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				if err := ValidateCreateUploadReplay(record, 7, record.IdempotencyKey, record.RequestHash, now); err != nil {
					t.Error("并发复用校验失败", err)
					return
				}
			}
		}()
	}
	workers.Wait()
	if !reflect.DeepEqual(record, &original) {
		t.Fatal("复用校验修改了原始记录")
	}
}
