package dto

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func createdDirectoryDataFixture() *domain.FileEntry {
	parentID := uint64(11)
	stamp := time.Date(2026, 11, 16, 9, 30, 0, 123456789, time.FixedZone("fixture", 8*3600))
	return &domain.FileEntry{
		ID: 42, UserID: 7, ParentID: &parentID, Name: "学习资料 🌱", EntryType: "DIR", Revision: 1, Status: "ACTIVE",
		CreatedAt: stamp, ModifiedAt: stamp.Add(time.Second), UpdatedAt: stamp.Add(2 * time.Second),
	}
}

func TestNewCreateDirectoryDataJSON(t *testing.T) {
	empty, green := "", "green"
	for _, color := range []*string{nil, &empty, &green} {
		result := createdDirectoryDataFixture()
		data, err := NewCreateDirectoryData(result, color)
		if err != nil || data == nil {
			t.Fatal("创建结果 DTO 转换失败", err)
		}
		response := CreateDirectoryResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		var outer map[string]json.RawMessage
		if json.Unmarshal(encoded, &outer) != nil || len(outer) != 2 || string(outer["request_id"]) != `"28ee758a-0727-42de-ae7d-3bb33155e148"` {
			t.Fatal("响应外层字段错误")
		}
		var payload map[string]json.RawMessage
		if json.Unmarshal(outer["data"], &payload) != nil || len(payload) != 1 {
			t.Fatal("创建数据结构错误")
		}
		var entry map[string]any
		if json.Unmarshal(payload["entry"], &entry) != nil {
			t.Fatal("目录快照编码错误")
		}
		var expectedColor any
		if color != nil {
			expectedColor = *color
		}
		expected := map[string]any{
			"id": "42", "parent_id": "11", "name": "学习资料 🌱", "entry_type": "DIR", "size_bytes": float64(0), "revision": "1",
			"created_at": "2026-11-16T01:30:00.123456Z", "modified_at": "2026-11-16T01:30:01.123456Z", "updated_at": "2026-11-16T01:30:02.123456Z",
			"status": "ACTIVE", "color_key": expectedColor,
		}
		if len(entry) != 11 || !reflect.DeepEqual(entry, expected) {
			t.Fatal("十一项公开字段、空值或时间错误")
		}
	}
}

func TestNewCreateDirectoryDataBounds(t *testing.T) {
	result := createdDirectoryDataFixture()
	maximum := ^uint64(0)
	parentID := maximum - 1
	result.ID, result.UserID, result.ParentID = maximum, maximum, &parentID
	result.Name = "Cafe\u0301"
	color := " Sand-2_ "
	data, err := NewCreateDirectoryData(result, &color)
	if err != nil || data.Entry.ID != "18446744073709551615" || data.Entry.ParentID != "18446744073709551614" || data.Entry.Revision != "1" || data.Entry.Name != result.Name || data.Entry.ColorKey == nil || *data.Entry.ColorKey != color {
		t.Fatal("完整整数、展示原文或颜色原文错误", err)
	}
	for _, value := range []time.Time{
		time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
		time.Date(10000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 3600)),
		time.Date(999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600)),
	} {
		result.CreatedAt, result.ModifiedAt, result.UpdatedAt = value, value, value
		data, err := NewCreateDirectoryData(result, nil)
		if err != nil || data == nil {
			t.Fatal("合法 UTC 年份边界被拒绝", err)
		}
		for _, timestamp := range []time.Time{data.Entry.CreatedAt, data.Entry.ModifiedAt, data.Entry.UpdatedAt} {
			if !timestamp.Equal(value.UTC().Truncate(time.Microsecond)) || timestamp.Location() != time.UTC || timestamp.Nanosecond()%1000 != 0 {
				t.Fatal("时间未正确复制为 UTC 微秒")
			}
		}
		if _, err := json.Marshal(data); err != nil {
			t.Fatal("合法边界无法编码", err)
		}
	}
}

func TestNewCreateDirectoryDataFailures(t *testing.T) {
	if data, err := NewCreateDirectoryData(nil, nil); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
		t.Fatal("空创建结果未拒绝", err)
	}
	for _, testCase := range []struct {
		name   string
		change func(*domain.FileEntry)
	}{
		{name: "zero id", change: func(entry *domain.FileEntry) { entry.ID = 0 }},
		{name: "zero owner", change: func(entry *domain.FileEntry) { entry.UserID = 0 }},
		{name: "root parent", change: func(entry *domain.FileEntry) { entry.ParentID = nil }},
		{name: "zero parent", change: func(entry *domain.FileEntry) { *entry.ParentID = 0 }},
		{name: "self parent", change: func(entry *domain.FileEntry) { *entry.ParentID = entry.ID }},
		{name: "empty name", change: func(entry *domain.FileEntry) { entry.Name = "" }},
		{name: "invalid utf8 name", change: func(entry *domain.FileEntry) { entry.Name = "private-name\xff" }},
		{name: "file result", change: func(entry *domain.FileEntry) { entry.EntryType = "FILE" }},
		{name: "nonzero bytes", change: func(entry *domain.FileEntry) { entry.FileSize = 1 }},
		{name: "zero revision", change: func(entry *domain.FileEntry) { entry.Revision = 0 }},
		{name: "later revision", change: func(entry *domain.FileEntry) { entry.Revision = 2 }},
		{name: "trashed", change: func(entry *domain.FileEntry) { entry.Status = "TRASHED" }},
		{name: "purging", change: func(entry *domain.FileEntry) { entry.Status = "PURGING" }},
		{name: "purged", change: func(entry *domain.FileEntry) { entry.Status = "PURGED" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := createdDirectoryDataFixture()
			testCase.change(result)
			if data, err := NewCreateDirectoryData(result, nil); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常目录快照未返回空结果与领域错误", err)
			}
		})
	}
	for _, color := range []string{strings.Repeat("a", 17), "绿色", "private-color\n"} {
		if data, err := NewCreateDirectoryData(createdDirectoryDataFixture(), &color); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
			t.Fatal("异常颜色未作为目录数据错误处理", err)
		}
	}
	for _, value := range []time.Time{
		{}, time.Date(999, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 3600)),
		time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600)),
	} {
		for _, field := range []string{"created", "modified", "updated"} {
			result := createdDirectoryDataFixture()
			switch field {
			case "created":
				result.CreatedAt = value
			case "modified":
				result.ModifiedAt = value
			case "updated":
				result.UpdatedAt = value
			}
			if data, err := NewCreateDirectoryData(result, nil); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常 UTC 时间未拒绝", field, err)
			}
		}
	}
}

func TestNewCreateDirectoryDataStableSnapshot(t *testing.T) {
	result := createdDirectoryDataFixture()
	color := "green"
	data, err := NewCreateDirectoryData(result, &color)
	if err != nil {
		t.Fatal(err)
	}
	stable, err := json.Marshal(data)
	if err != nil || bytes.Contains(stable, []byte("request_id")) || bytes.Contains(stable, []byte("user_id")) || bytes.Contains(stable, []byte("name_key")) {
		t.Fatal("稳定数据字段或编码错误", err)
	}
	result.Name, *result.ParentID, color = "当前目录已改名", 999, "sand"
	result.CreatedAt = result.CreatedAt.Add(24 * time.Hour)
	if data.Entry.Name != "学习资料 🌱" || data.Entry.ParentID != "11" || data.Entry.ColorKey == nil || *data.Entry.ColorKey != "green" {
		t.Fatal("创建快照共享了可变输入")
	}
	var restored CreateDirectoryData
	if json.Unmarshal(stable, &restored) != nil || !reflect.DeepEqual(data, &restored) {
		t.Fatal("稳定数据序列化后无法恢复")
	}
	first, firstError := json.Marshal(CreateDirectoryResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"})
	next, nextError := json.Marshal(CreateDirectoryResponse{Data: restored, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e149"})
	if firstError != nil || nextError != nil || bytes.Equal(first, next) {
		t.Fatal("响应外层未关联各自请求 ID")
	}
	current, err := json.Marshal(data)
	if err != nil || !bytes.Equal(stable, current) {
		t.Fatal("外层交付影响稳定结果数据", err)
	}
}
