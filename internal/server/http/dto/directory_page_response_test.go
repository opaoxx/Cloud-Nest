package dto

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func directoryPageDataFixture() *domain.DirectoryPageResult {
	rootID, parentID := uint64(11), uint64(12)
	stamp := time.Date(2026, 11, 15, 9, 30, 0, 123456789, time.FixedZone("fixture", 8*3600))
	root := domain.FileEntry{ID: rootID, UserID: 7, EntryType: "DIR", Revision: 1, Status: "ACTIVE"}
	directory := root
	directory.ID, directory.ParentID, directory.Name = parentID, &rootID, "课程资料"
	file := domain.FileEntry{ID: 23, UserID: 7, ParentID: &parentID, Name: "学习笔记.md", EntryType: "FILE", FileSize: 2048, Revision: 3, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: stamp.Add(time.Hour), UpdatedAt: stamp.Add(time.Hour)}
	return &domain.DirectoryPageResult{Path: domain.FileEntryPath{Entries: []domain.FileEntry{root, directory}}, Entries: []domain.FileEntry{file}}
}

func TestNewDirectoryPageDataJSON(t *testing.T) {
	result := directoryPageDataFixture()
	data, err := NewDirectoryPageData(result)
	if err != nil || data == nil {
		t.Fatal(err)
	}
	response := DirectoryPageResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var outer map[string]json.RawMessage
	if json.Unmarshal(encoded, &outer) != nil || len(outer) != 2 || string(outer["request_id"]) != `"28ee758a-0727-42de-ae7d-3bb33155e148"` {
		t.Fatal("目录响应外层错误")
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(outer["data"], &payload) != nil || len(payload) != 3 || string(payload["next_cursor"]) != "null" {
		t.Fatal("目录数据或可空游标错误")
	}
	var items []map[string]any
	if json.Unmarshal(payload["items"], &items) != nil || len(items) != 1 || len(items[0]) != 10 {
		t.Fatal("基础子项字段数量错误")
	}
	item := items[0]
	if item["id"] != "23" || item["parent_id"] != "12" || item["name"] != "学习笔记.md" || item["entry_type"] != "FILE" || item["size_bytes"] != float64(2048) || item["revision"] != "3" || item["status"] != "ACTIVE" {
		t.Fatal("子项字符串 ID、版本或展示字段错误")
	}
	if item["created_at"] != "2026-11-15T01:30:00.123456Z" || item["modified_at"] != "2026-11-15T02:30:00.123456Z" || item["updated_at"] != "2026-11-15T02:30:00.123456Z" {
		t.Fatal("UTC 微秒时间错误")
	}
	var breadcrumbs []map[string]any
	if json.Unmarshal(payload["breadcrumbs"], &breadcrumbs) != nil || len(breadcrumbs) != 2 || len(breadcrumbs[0]) != 2 || breadcrumbs[0]["id"] != "11" || breadcrumbs[0]["name"] != "" || breadcrumbs[1]["id"] != "12" || breadcrumbs[1]["name"] != "课程资料" {
		t.Fatal("面包屑字段或真实根目录名称错误")
	}
	for _, stamp := range []time.Time{data.Items[0].CreatedAt, data.Items[0].ModifiedAt, data.Items[0].UpdatedAt} {
		if stamp.Location() != time.UTC || stamp.Nanosecond()%1000 != 0 {
			t.Fatal("时间未规范为 UTC 微秒")
		}
	}
}

func TestNewDirectoryPageDataEmptyAndBounds(t *testing.T) {
	for _, rootOnly := range []bool{false, true} {
		result := directoryPageDataFixture()
		result.Entries = nil
		if rootOnly {
			result.Path.Entries = result.Path.Entries[:1]
		}
		data, err := NewDirectoryPageData(result)
		if err != nil || data == nil || data.Items == nil || len(data.Items) != 0 || data.NextCursor != nil {
			t.Fatal("空页或根目录转换失败", err)
		}
		encoded, err := json.Marshal(data)
		if err != nil || !strings.Contains(string(encoded), `"items":[]`) || !strings.Contains(string(encoded), `"next_cursor":null`) {
			t.Fatal("空数组或 null 被省略")
		}
	}
	t.Run("100 items and directory zero size", func(t *testing.T) {
		result := directoryPageDataFixture()
		base := result.Entries[0]
		base.EntryType, base.FileSize = "DIR", 0
		result.Entries = make([]domain.FileEntry, 100)
		for index := range result.Entries {
			result.Entries[index] = base
			result.Entries[index].ID = uint64(200 - index)
		}
		cursor := "fixture-signed-cursor"
		result.NextCursor = &cursor
		data, err := NewDirectoryPageData(result)
		if err != nil || data == nil || len(data.Items) != 100 || data.Items[0].SizeBytes != 0 || data.Items[0].EntryType != "DIR" || data.NextCursor == nil {
			t.Fatal("百条目录页错误", err)
		}
		encoded, err := json.Marshal(data.Items[0])
		if err != nil || !strings.Contains(string(encoded), `"size_bytes":0`) {
			t.Fatal("目录零大小未明确交付")
		}
	})
	t.Run("full identifiers revision and safe bytes", func(t *testing.T) {
		result := directoryPageDataFixture()
		maximum := ^uint64(0)
		rootID, parentID := maximum-2, maximum-1
		result.Path.Entries[0].ID, result.Path.Entries[0].UserID = rootID, maximum
		result.Path.Entries[1].ID, result.Path.Entries[1].UserID, result.Path.Entries[1].ParentID = parentID, maximum, &rootID
		result.Entries[0].ID, result.Entries[0].UserID, result.Entries[0].ParentID, result.Entries[0].Revision, result.Entries[0].FileSize = maximum, maximum, &parentID, maximum, maxJSONSafeInteger
		data, err := NewDirectoryPageData(result)
		if err != nil || data == nil || data.Items[0].ID != "18446744073709551615" || data.Items[0].Revision != "18446744073709551615" || data.Items[0].ParentID != strconv.FormatUint(parentID, 10) || data.Items[0].SizeBytes != maxJSONSafeInteger || data.Breadcrumbs[0].ID != strconv.FormatUint(rootID, 10) {
			t.Fatal("完整标识、版本或安全字节上限错误", err)
		}
	})
	t.Run("maximum breadcrumb depth", func(t *testing.T) {
		result := directoryPageDataFixture()
		result.Entries = nil
		root := result.Path.Entries[0]
		result.Path.Entries = []domain.FileEntry{root}
		for depth := 1; depth <= 32; depth++ {
			entry := root
			parentID := result.Path.Entries[len(result.Path.Entries)-1].ID
			entry.ID, entry.ParentID, entry.Name = uint64(100+depth), &parentID, strconv.Itoa(depth)
			result.Path.Entries = append(result.Path.Entries, entry)
		}
		data, err := NewDirectoryPageData(result)
		if err != nil || data == nil || len(data.Breadcrumbs) != 33 {
			t.Fatal("最大面包屑深度错误", err)
		}
	})
	for _, stamp := range []time.Time{time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)} {
		result := directoryPageDataFixture()
		result.Entries[0].CreatedAt, result.Entries[0].ModifiedAt, result.Entries[0].UpdatedAt = stamp, stamp, stamp
		if data, err := NewDirectoryPageData(result); err != nil || data == nil || !data.Items[0].ModifiedAt.Equal(stamp) {
			t.Fatal("数据库时间边界转换失败", err)
		}
	}
}

func TestNewDirectoryPageDataFailures(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		change       func(*domain.DirectoryPageResult)
		numericError bool
	}{
		{name: "empty path", change: func(result *domain.DirectoryPageResult) { result.Path.Entries = nil }},
		{name: "oversize path", change: func(result *domain.DirectoryPageResult) { result.Path.Entries = make([]domain.FileEntry, 34) }},
		{name: "oversize page", change: func(result *domain.DirectoryPageResult) { result.Entries = make([]domain.FileEntry, 101) }},
		{name: "empty cursor", change: func(result *domain.DirectoryPageResult) { cursor := ""; result.NextCursor = &cursor }},
		{name: "long cursor", change: func(result *domain.DirectoryPageResult) {
			cursor := strings.Repeat("x", domain.MaxDirectoryCursorLength+1)
			result.NextCursor = &cursor
		}},
		{name: "cursor with empty page", change: func(result *domain.DirectoryPageResult) {
			cursor := "next"
			result.NextCursor = &cursor
			result.Entries = nil
		}},
		{name: "zero owner", change: func(result *domain.DirectoryPageResult) { result.Path.Entries[0].UserID = 0 }},
		{name: "zero breadcrumb id", change: func(result *domain.DirectoryPageResult) { result.Path.Entries[1].ID = 0 }},
		{name: "breadcrumb owner", change: func(result *domain.DirectoryPageResult) { result.Path.Entries[1].UserID = 8 }},
		{name: "breadcrumb file", change: func(result *domain.DirectoryPageResult) { result.Path.Entries[1].EntryType = "FILE" }},
		{name: "inactive breadcrumb", change: func(result *domain.DirectoryPageResult) { result.Path.Entries[1].Status = "TRASHED" }},
		{name: "named root", change: func(result *domain.DirectoryPageResult) { result.Path.Entries[0].Name = "伪根目录" }},
		{name: "root parent", change: func(result *domain.DirectoryPageResult) {
			parent := uint64(99)
			result.Path.Entries[0].ParentID = &parent
		}},
		{name: "missing path parent", change: func(result *domain.DirectoryPageResult) { result.Path.Entries[1].ParentID = nil }},
		{name: "wrong path link", change: func(result *domain.DirectoryPageResult) {
			parent := uint64(99)
			result.Path.Entries[1].ParentID = &parent
		}},
		{name: "empty path name", change: func(result *domain.DirectoryPageResult) { result.Path.Entries[1].Name = "" }},
		{name: "zero item id", change: func(result *domain.DirectoryPageResult) { result.Entries[0].ID = 0 }},
		{name: "wrong item owner", change: func(result *domain.DirectoryPageResult) { result.Entries[0].UserID = 8 }},
		{name: "null item parent", change: func(result *domain.DirectoryPageResult) { result.Entries[0].ParentID = nil }},
		{name: "wrong item parent", change: func(result *domain.DirectoryPageResult) { parent := uint64(99); result.Entries[0].ParentID = &parent }},
		{name: "zero revision", change: func(result *domain.DirectoryPageResult) { result.Entries[0].Revision = 0 }},
		{name: "empty item name", change: func(result *domain.DirectoryPageResult) { result.Entries[0].Name = "" }},
		{name: "inactive item", change: func(result *domain.DirectoryPageResult) { result.Entries[0].Status = "PURGING" }},
		{name: "unknown type", change: func(result *domain.DirectoryPageResult) { result.Entries[0].EntryType = "UNKNOWN" }},
		{name: "directory size", change: func(result *domain.DirectoryPageResult) { result.Entries[0].EntryType = "DIR" }},
		{name: "unsafe bytes", change: func(result *domain.DirectoryPageResult) { result.Entries[0].FileSize = maxJSONSafeInteger + 1 }, numericError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := directoryPageDataFixture()
			testCase.change(result)
			data, err := NewDirectoryPageData(result)
			if data != nil || err == nil || (!testCase.numericError && !errors.Is(err, domain.ErrInvalidFileEntryData)) {
				t.Fatal("异常结果未返回空 DTO 或错误分类不准确", err)
			}
		})
	}
	if data, err := NewDirectoryPageData(nil); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
		t.Fatal("空结果仍转换成功")
	}
	for _, field := range []string{"created", "modified", "updated"} {
		for _, stamp := range []time.Time{time.Time{}, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1000, 1, 1, 0, 0, 0, 0, time.FixedZone("fixture", 3600))} {
			result := directoryPageDataFixture()
			switch field {
			case "created":
				result.Entries[0].CreatedAt = stamp
			case "modified":
				result.Entries[0].ModifiedAt = stamp
			case "updated":
				result.Entries[0].UpdatedAt = stamp
			}
			if data, err := NewDirectoryPageData(result); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常 UTC 时间未拒绝", field, err)
			}
		}
	}
	result := directoryPageDataFixture()
	bad := result.Entries[0]
	bad.ID = 24
	bad.ModifiedAt = time.Time{}
	result.Entries = append(result.Entries, bad)
	if data, err := NewDirectoryPageData(result); data != nil || err == nil {
		t.Fatal("后项异常仍返回部分 DTO")
	}
}

func TestNewDirectoryPageDataCopies(t *testing.T) {
	result := directoryPageDataFixture()
	cursor := "fixture-signed-cursor"
	result.NextCursor = &cursor
	first, err := NewDirectoryPageData(result)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewDirectoryPageData(result)
	if err != nil {
		t.Fatal(err)
	}
	original := *second
	original.Items = append([]DirectoryEntrySummary{}, second.Items...)
	original.Breadcrumbs = append([]DirectoryBreadcrumb{}, second.Breadcrumbs...)
	copiedCursor := *second.NextCursor
	original.NextCursor = &copiedCursor
	result.Entries[0].Name, result.Path.Entries[1].Name, *result.NextCursor = "来源子项", "来源路径", "来源游标"
	if first.Items[0].Name != "学习笔记.md" || first.Breadcrumbs[1].Name != "课程资料" || *first.NextCursor != "fixture-signed-cursor" {
		t.Fatal("来源修改影响 DTO")
	}
	first.Items[0].Name, first.Breadcrumbs[1].Name, *first.NextCursor = "DTO 子项", "DTO 路径", "DTO 游标"
	if !reflect.DeepEqual(*second, original) {
		t.Fatal("重复转换结果共享可变数据")
	}
}
