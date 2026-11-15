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

func createDirectoryReplayFixture(t *testing.T) (*domain.IdempotencyRecord, time.Time) {
	t.Helper()
	hash, err := BuildCreateDirectoryRequestHash(11, "学习资料", nil)
	if err != nil {
		t.Fatal(err)
	}
	resourceID := uint64(42)
	createdAt := time.Date(2026, 11, 16, 1, 30, 0, 123456000, time.UTC)
	return &domain.IdempotencyRecord{
		ID: 23, UserID: 7, Operation: domain.CreateDirectoryOperation, IdempotencyKey: "request-007", RequestHash: hash,
		ResourceType: "ENTRY", ResourceID: &resourceID, ResponseStatus: 201,
		ResponseBody: json.RawMessage(`{"entry":{"id":"42","parent_id":"11","name":"学习资料","entry_type":"DIR","size_bytes":0,"revision":"1","created_at":"2026-11-16T01:30:00.123456Z","modified_at":"2026-11-16T01:30:00.123456Z","updated_at":"2026-11-16T01:30:00.123456Z","status":"ACTIVE","color_key":null}}`),
		CreatedAt:    createdAt, ExpiresAt: createdAt.Add(8 * 24 * time.Hour),
	}, createdAt.Add(time.Hour)
}

func TestValidateCreateDirectoryReplaySuccess(t *testing.T) {
	for _, name := range []string{"ordinary", "deadline minus nanosecond", "timezone", "maximum ids", "minimum key", "maximum key", "date bounds", "reformatted json"} {
		t.Run(name, func(t *testing.T) {
			record, now := createDirectoryReplayFixture(t)
			switch name {
			case "deadline minus nanosecond":
				now = record.ExpiresAt.Add(-time.Nanosecond)
			case "timezone":
				zone := time.FixedZone("UTC+8", 8*3600)
				record.CreatedAt, record.ExpiresAt, now = record.CreatedAt.In(zone), record.ExpiresAt.In(zone), now.In(zone)
			case "maximum ids":
				record.ID, record.UserID, *record.ResourceID = ^uint64(0), ^uint64(0), ^uint64(0)
				record.ResponseBody = json.RawMessage(strings.Replace(string(record.ResponseBody), `"id":"42"`, `"id":"18446744073709551615"`, 1))
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
			if err := ValidateCreateDirectoryReplay(record, record.UserID, record.IdempotencyKey, record.RequestHash, now); err != nil {
				t.Fatal("合法同键同请求未通过", err)
			}
		})
	}
}

func TestValidateCreateDirectoryReplayInvalidRecords(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		change func(*domain.IdempotencyRecord)
	}{
		{"zero record id", func(record *domain.IdempotencyRecord) { record.ID = 0 }},
		{"other user", func(record *domain.IdempotencyRecord) { record.UserID = 8 }},
		{"other operation", func(record *domain.IdempotencyRecord) { record.Operation = "v2.directories.create" }},
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
		{"other resource type", func(record *domain.IdempotencyRecord) { record.ResourceType = "UPLOAD_TASK" }},
		{"missing resource", func(record *domain.IdempotencyRecord) { record.ResourceID = nil }},
		{"zero resource", func(record *domain.IdempotencyRecord) { *record.ResourceID = 0 }},
		{"other status", func(record *domain.IdempotencyRecord) { record.ResponseStatus = 200 }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			record, now := createDirectoryReplayFixture(t)
			testCase.change(record)
			if err := ValidateCreateDirectoryReplay(record, 7, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || err.Error() != "幂等记录数据异常" {
				t.Fatal("异常记录未返回固定数据错误", err)
			}
		})
	}
	record, now := createDirectoryReplayFixture(t)
	if err := ValidateCreateDirectoryReplay(nil, 7, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
		t.Fatal("成功读取后的空记录未拒绝", err)
	}
	for _, body := range []string{
		"", "null", "[]", "private-json", `{"entry":null}`, `{"entry":{}}`, `{"entry":[]}`,
		`{"entry":{"id":42}}`, `{"entry":{"id":"0"}}`, `{"entry":{"id":"042"}}`,
		`{"entry":{"id":"43"}}`, `{"entry":{"id":"18446744073709551616"}}`,
		`{"entry":{"id":"42"}} {}`, "{\"entry\":{\"id\":\"42\",\"name\":\"private\xff\"}}",
	} {
		record.ResponseBody = json.RawMessage(body)
		if err := ValidateCreateDirectoryReplay(record, 7, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
			t.Fatal("正文语法或资源标识错误未拒绝", err)
		}
	}
	record.ResponseBody = nil
	if err := ValidateCreateDirectoryReplay(record, 7, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
		t.Fatal("SQL NULL 正文未拒绝", err)
	}
}

func TestValidateCreateDirectoryReplayInputs(t *testing.T) {
	record, now := createDirectoryReplayFixture(t)
	for _, key := range []string{"", " ", "a,b", "a b", "key\t", "中文", strings.Repeat("a", 129)} {
		if err := ValidateCreateDirectoryReplay(record, 7, key, record.RequestHash, now); err == nil || errors.Is(err, domain.ErrIdempotencyRequestConflict) || errors.Is(err, domain.ErrIdempotencyRecordExpired) {
			t.Fatal("无效调用键未作为调用错误处理", err)
		}
	}
	if err := ValidateCreateDirectoryReplay(record, 0, record.IdempotencyKey, record.RequestHash, now); err == nil {
		t.Fatal("零用户未拒绝")
	}
	if err := ValidateCreateDirectoryReplay(record, 7, record.IdempotencyKey, record.RequestHash, time.Time{}); err == nil {
		t.Fatal("零判断时间未拒绝")
	}
}

func TestValidateCreateDirectoryReplayConflictAndExpiry(t *testing.T) {
	record, now := createDirectoryReplayFixture(t)
	empty, green := "", "green"
	for _, request := range []struct {
		name   string
		parent uint64
		color  *string
	}{
		{"学习资料", 11, nil}, {"  学习资料\u3000", 11, nil},
		{"其他资料", 11, nil}, {"学习资料", 12, nil}, {"学习资料", 11, &empty}, {"学习资料", 11, &green},
	} {
		hash, err := BuildCreateDirectoryRequestHash(request.parent, request.name, request.color)
		if err != nil {
			t.Fatal(err)
		}
		err = ValidateCreateDirectoryReplay(record, 7, record.IdempotencyKey, hash, now)
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
			if err := ValidateCreateDirectoryReplay(record, 7, record.IdempotencyKey, hash, checkTime); !errors.Is(err, domain.ErrIdempotencyRecordExpired) {
				t.Fatal("到期判断或到期优先顺序错误", err)
			}
		}
	}
	// 范围异常先于到期，摘要冲突先于具体结果读取。
	record.UserID = 8
	if err := ValidateCreateDirectoryReplay(record, 7, record.IdempotencyKey, record.RequestHash, record.ExpiresAt); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
		t.Fatal("范围异常被误判为到期", err)
	}
	record.UserID, record.ResponseBody = 7, nil
	hash := record.RequestHash
	hash[31] ^= 0xff
	if err := ValidateCreateDirectoryReplay(record, 7, record.IdempotencyKey, hash, now); !errors.Is(err, domain.ErrIdempotencyRequestConflict) {
		t.Fatal("摘要冲突判断顺序错误", err)
	}
}

func TestValidateCreateDirectoryReplayIndependent(t *testing.T) {
	record, now := createDirectoryReplayFixture(t)
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
				if err := ValidateCreateDirectoryReplay(record, 7, record.IdempotencyKey, record.RequestHash, now); err != nil {
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
