package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func moveEntryReplayFixture(t *testing.T) (*domain.IdempotencyRecord, time.Time) {
	t.Helper()
	hash, err := BuildMoveEntryRequestHash(42, 3, 19)
	if err != nil {
		t.Fatal(err)
	}
	resourceID := uint64(42)
	createdAt := time.Date(2026, 11, 16, 1, 30, 0, 123456000, time.UTC)
	return &domain.IdempotencyRecord{
		ID: 23, UserID: 7, Operation: domain.MoveEntryOperation, IdempotencyKey: "request-007", RequestHash: hash,
		ResourceType: "ENTRY", ResourceID: &resourceID, ResponseStatus: 200,
		// 夹具提供资源投影，完整快照由真实数据库联测核对。
		ResponseBody: json.RawMessage(`{"entry":{"id":"42","parent_id":"19","revision":"4"}}`),
		CreatedAt:    createdAt, ExpiresAt: createdAt.Add(8 * 24 * time.Hour),
	}, createdAt.Add(time.Hour)
}

func TestValidateMoveEntryReplaySuccess(t *testing.T) {
	for _, mode := range []string{"ordinary", "deadline minus nanosecond", "timezone", "maximum ids", "minimum key", "maximum key", "date bounds", "reformatted json", "minimal projection"} {
		t.Run(mode, func(t *testing.T) {
			record, now := moveEntryReplayFixture(t)
			targetParentID := uint64(19)
			switch mode {
			case "deadline minus nanosecond":
				now = record.ExpiresAt.Add(-time.Nanosecond)
			case "timezone":
				zone := time.FixedZone("UTC+8", 8*3600)
				record.CreatedAt, record.ExpiresAt, now = record.CreatedAt.In(zone), record.ExpiresAt.In(zone), now.In(zone)
			case "maximum ids":
				record.ID, record.UserID, *record.ResourceID = ^uint64(0), ^uint64(0), ^uint64(0)
				targetParentID = ^uint64(0) - 1
				record.ResponseBody = json.RawMessage(`{"entry":{"id":"18446744073709551615","parent_id":"18446744073709551614"}}`)
				var err error
				record.RequestHash, err = BuildMoveEntryRequestHash(*record.ResourceID, 3, targetParentID)
				if err != nil {
					t.Fatal(err)
				}
			case "minimum key":
				record.IdempotencyKey = "!"
			case "maximum key":
				record.IdempotencyKey = strings.Repeat("~", 128)
			case "date bounds":
				record.CreatedAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				record.ExpiresAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
			case "reformatted json":
				var formatted bytes.Buffer
				if err := json.Indent(&formatted, record.ResponseBody, "", "  "); err != nil {
					t.Fatal(err)
				}
				record.ResponseBody = formatted.Bytes()
			case "minimal projection":
				// 本层确认源和父目录标识，完整十八项字段由恢复入口继续核对。
				record.ResponseBody = json.RawMessage(`{"entry":{"id":"42","parent_id":"19"}}`)
			}
			if err := ValidateMoveEntryReplay(record, record.UserID, *record.ResourceID, targetParentID, record.IdempotencyKey, record.RequestHash, now); err != nil {
				t.Fatal("合法复用结果未通过", err)
			}
		})
	}
}

func TestValidateMoveEntryReplayInvalidRecords(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		change func(*domain.IdempotencyRecord)
	}{
		{"zero record id", func(record *domain.IdempotencyRecord) { record.ID = 0 }},
		{"other user", func(record *domain.IdempotencyRecord) { record.UserID++ }},
		{"other operation", func(record *domain.IdempotencyRecord) { record.Operation = domain.RenameEntryOperation }},
		{"other key", func(record *domain.IdempotencyRecord) { record.IdempotencyKey = "other-key" }},
		{"key case", func(record *domain.IdempotencyRecord) { record.IdempotencyKey = "REQUEST-007" }},
		{"zero creation", func(record *domain.IdempotencyRecord) { record.CreatedAt = time.Time{} }},
		{"zero expiration", func(record *domain.IdempotencyRecord) { record.ExpiresAt = time.Time{} }},
		{"equal times", func(record *domain.IdempotencyRecord) { record.ExpiresAt = record.CreatedAt }},
		{"expiration precedes creation", func(record *domain.IdempotencyRecord) { record.ExpiresAt = record.CreatedAt.Add(-time.Microsecond) }},
		{"creation before range", func(record *domain.IdempotencyRecord) {
			record.CreatedAt = time.Date(999, 12, 31, 23, 59, 59, 0, time.UTC)
		}},
		{"expiration after range", func(record *domain.IdempotencyRecord) {
			record.ExpiresAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		}},
		{"utc overflow", func(record *domain.IdempotencyRecord) {
			record.ExpiresAt = time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600))
		}},
		{"other resource", func(record *domain.IdempotencyRecord) { record.ResourceType = "UPLOAD_TASK" }},
		{"missing target", func(record *domain.IdempotencyRecord) { record.ResourceID = nil }},
		{"zero target", func(record *domain.IdempotencyRecord) { *record.ResourceID = 0 }},
		{"other target", func(record *domain.IdempotencyRecord) { *record.ResourceID = 43 }},
		{"creation status", func(record *domain.IdempotencyRecord) { record.ResponseStatus = 201 }},
		{"empty status", func(record *domain.IdempotencyRecord) { record.ResponseStatus = 204 }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			record, now := moveEntryReplayFixture(t)
			testCase.change(record)
			if err := ValidateMoveEntryReplay(record, 7, 42, 19, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || err.Error() != "幂等记录数据异常" {
				t.Fatal("异常记录未返回固定数据错误", err)
			}
		})
	}
	record, now := moveEntryReplayFixture(t)
	if err := ValidateMoveEntryReplay(nil, 7, 42, 19, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
		t.Fatal("空记录未拒绝", err)
	}
	for _, body := range []string{
		"", "null", "[]", "private-json", `{"entry":null}`, `{"entry":{}}`, `{"entry":[]}`,
		`{"entry":{"id":42,"parent_id":"19"}}`, `{"entry":{"id":"0","parent_id":"19"}}`,
		`{"entry":{"id":"042","parent_id":"19"}}`, `{"entry":{"id":"43","parent_id":"19"}}`,
		`{"entry":{"id":"18446744073709551616","parent_id":"19"}}`,
		`{"entry":{"id":"42","parent_id":"19"}} {}`,
		"{\"entry\":{\"id\":\"42\",\"parent_id\":\"19\",\"name\":\"private\xff\"}}",
		`{"entry":{"id":"42"}}`, `{"entry":{"id":"42","parent_id":null}}`,
		`{"entry":{"id":"42","parent_id":19}}`, `{"entry":{"id":"42","parent_id":"0"}}`,
		`{"entry":{"id":"42","parent_id":"019"}}`, `{"entry":{"id":"42","parent_id":"42"}}`,
		`{"entry":{"id":"42","parent_id":"20"}}`, `{"entry":{"id":"42","parent_id":"18446744073709551616"}}`,
	} {
		record.ResponseBody = json.RawMessage(body)
		if err := ValidateMoveEntryReplay(record, 7, 42, 19, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
			t.Fatal("正文或目标标识未拒绝", err)
		}
	}
	record.ResponseBody = nil
	if err := ValidateMoveEntryReplay(record, 7, 42, 19, "request-007", record.RequestHash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
		t.Fatal("SQL NULL 正文未拒绝", err)
	}
	t.Run("identical source and parent", func(t *testing.T) {
		record, now := moveEntryReplayFixture(t)
		record.ResponseBody = json.RawMessage(`{"entry":{"id":"42","parent_id":"42"}}`)
		hash, err := BuildMoveEntryRequestHash(42, 3, 42)
		if err != nil {
			t.Fatal(err)
		}
		record.RequestHash = hash
		if err := ValidateMoveEntryReplay(record, 7, 42, 42, record.IdempotencyKey, hash, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
			t.Fatal("自身父项结果未拒绝", err)
		}
	})
}

func TestValidateMoveEntryReplayInputs(t *testing.T) {
	record, now := moveEntryReplayFixture(t)
	for _, key := range []string{"", " ", "a,b", "a b", "key\t", "中文", strings.Repeat("a", 129)} {
		if err := ValidateMoveEntryReplay(record, 7, 42, 19, key, record.RequestHash, now); err == nil || errors.Is(err, domain.ErrIdempotencyRequestConflict) || errors.Is(err, domain.ErrIdempotencyRecordExpired) {
			t.Fatal("无效键未作为调用错误处理", err)
		}
	}
	for _, scope := range []struct{ user, entry uint64 }{{0, 42}, {7, 0}} {
		if err := ValidateMoveEntryReplay(record, scope.user, scope.entry, 19, record.IdempotencyKey, record.RequestHash, now); err == nil {
			t.Fatal("零标识未拒绝")
		}
	}
	if err := ValidateMoveEntryReplay(record, 7, 42, 0, record.IdempotencyKey, record.RequestHash, now); err == nil {
		t.Fatal("零目标父目录未拒绝")
	}
	if err := ValidateMoveEntryReplay(record, 7, 42, 19, record.IdempotencyKey, record.RequestHash, time.Time{}); err == nil {
		t.Fatal("零判断时间未拒绝")
	}
}

func TestValidateMoveEntryReplayConflictAndExpiry(t *testing.T) {
	record, now := moveEntryReplayFixture(t)
	for _, request := range []struct{ entry, revision, parent uint64 }{
		{42, 3, 19}, {43, 3, 19}, {42, 4, 19}, {42, 3, 20}, {42, 3, 42},
	} {
		hash, err := BuildMoveEntryRequestHash(request.entry, request.revision, request.parent)
		if err != nil {
			t.Fatal(err)
		}
		err = ValidateMoveEntryReplay(record, 7, request.entry, request.parent, record.IdempotencyKey, hash, now)
		if hash == record.RequestHash {
			if err != nil {
				t.Fatal("相同请求未通过", err)
			}
		} else if !errors.Is(err, domain.ErrIdempotencyRequestConflict) || err.Error() != "幂等键已用于不同请求参数" {
			t.Fatal("参数变化未返回固定冲突", err)
		}
	}
	for _, checkTime := range []time.Time{record.ExpiresAt, record.ExpiresAt.Add(time.Nanosecond), record.ExpiresAt.Add(24 * time.Hour)} {
		for _, sameHash := range []bool{true, false} {
			hash := record.RequestHash
			if !sameHash {
				hash[0] ^= 0xff
			}
			if err := ValidateMoveEntryReplay(record, 7, 42, 19, record.IdempotencyKey, hash, checkTime); !errors.Is(err, domain.ErrIdempotencyRecordExpired) {
				t.Fatal("到期边界或判断顺序错误", err)
			}
		}
	}
	// 范围异常先于到期，摘要冲突先于资源和正文检查。
	if err := ValidateMoveEntryReplay(record, 8, 42, 19, record.IdempotencyKey, record.RequestHash, record.ExpiresAt); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
		t.Fatal("范围判断顺序错误", err)
	}
	record.ResourceID, record.ResponseBody = nil, nil
	hash := record.RequestHash
	hash[31] ^= 0xff
	if err := ValidateMoveEntryReplay(record, 7, 43, 20, record.IdempotencyKey, hash, now); !errors.Is(err, domain.ErrIdempotencyRequestConflict) {
		t.Fatal("摘要与资源判断顺序错误", err)
	}
}

func TestValidateMoveEntryReplayIndependent(t *testing.T) {
	record, now := moveEntryReplayFixture(t)
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
				if err := ValidateMoveEntryReplay(record, 7, 42, 19, record.IdempotencyKey, record.RequestHash, now); err != nil {
					t.Error("并发复用校验失败", err)
					return
				}
			}
		}()
	}
	workers.Wait()
	if !reflect.DeepEqual(record, &original) {
		t.Fatal("校验修改了原始记录")
	}
}
