package dto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func entryDetailDataFixture() *domain.EntryDetail {
	page := directoryPageDataFixture()
	entries := append(page.Path.Entries, page.Entries[0])
	for index := range entries {
		entries[index].CreatedAt = page.Entries[0].CreatedAt
		entries[index].ModifiedAt = page.Entries[0].ModifiedAt
		entries[index].UpdatedAt = page.Entries[0].UpdatedAt
	}
	category, extension, mimeType := "DOCUMENT", "md", "text/markdown"
	sourceID, sourceName := uint64(19), "  发布时设备 🎮  "
	contentAt := page.Entries[0].CreatedAt
	starredAt, openedAt := page.Entries[0].ModifiedAt, page.Entries[0].ModifiedAt.Add(time.Hour)
	return &domain.EntryDetail{
		Path: domain.FileEntryPath{Entries: entries},
		Display: domain.FileEntryDisplay{
			ID: 23, UserID: 7, Category: &category, Extension: &extension, MIMEType: &mimeType,
			SourceDeviceID: &sourceID, SourceDeviceName: &sourceName, ContentModifiedAt: &contentAt,
		},
		Preference: domain.EntryPreference{UserID: 7, EntryID: 23, StarredAt: &starredAt, LastOpenedAt: &openedAt},
	}
}

func TestNewEntryDetailDataJSON(t *testing.T) {
	data, err := NewEntryDetailData(entryDetailDataFixture())
	if err != nil || data == nil {
		t.Fatal(err)
	}
	response := EntryDetailResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var outer map[string]json.RawMessage
	if json.Unmarshal(encoded, &outer) != nil || len(outer) != 2 || string(outer["request_id"]) != `"28ee758a-0727-42de-ae7d-3bb33155e148"` {
		t.Fatal("详情响应外层错误")
	}
	var payload map[string]any
	if json.Unmarshal(outer["data"], &payload) != nil || len(payload) != 18 || payload["id"] != "23" || payload["parent_id"] != "12" ||
		payload["name"] != "学习笔记.md" || payload["kind"] != "document" || payload["size_bytes"] != float64(2048) ||
		payload["revision"] != "3" || payload["status"] != "ACTIVE" || payload["extension"] != "md" || payload["mime_type"] != "text/markdown" ||
		payload["color_key"] != nil || payload["starred"] != true {
		t.Fatal("详情公开字段、类型或数量错误")
	}
	for _, field := range []string{"created_at", "content_modified_at"} {
		if payload[field] != "2026-11-15T01:30:00.123456Z" {
			t.Fatal("创建或内容时间未规范化", field)
		}
	}
	for _, field := range []string{"modified_at", "updated_at", "starred_at"} {
		if payload[field] != "2026-11-15T02:30:00.123456Z" {
			t.Fatal("修改或收藏时间未规范化", field)
		}
	}
	if payload["last_opened_at"] != "2026-11-15T03:30:00.123456Z" {
		t.Fatal("最近访问时间错误")
	}
	var source map[string]any
	sourceJSON, err := json.Marshal(payload["source_device"])
	if err != nil || json.Unmarshal(sourceJSON, &source) != nil || len(source) != 2 || source["id"] != "19" || source["name"] != "  发布时设备 🎮  " {
		t.Fatal("来源设备字符串 ID 或发布名称原文错误")
	}
	for _, stamp := range []*time.Time{&data.CreatedAt, &data.ModifiedAt, &data.UpdatedAt, data.ContentModifiedAt, data.StarredAt, data.LastOpenedAt} {
		if stamp == nil || stamp.Location() != time.UTC || stamp.Nanosecond()%1000 != 0 {
			t.Fatal("详情时间未规范为 UTC 微秒")
		}
	}
}

func TestNewEntryDetailDataKindsAndNulls(t *testing.T) {
	for _, testCase := range []struct{ category, kind string }{
		{"DOCUMENT", "document"}, {"IMAGE", "image"}, {"VIDEO", "video"}, {"AUDIO", "audio"}, {"ARCHIVE", "archive"}, {"OTHER", "other"},
	} {
		t.Run(testCase.kind, func(t *testing.T) {
			result := entryDetailDataFixture()
			result.Display.Category = &testCase.category
			data, err := NewEntryDetailData(result)
			if err != nil || data == nil || data.Kind != testCase.kind {
				t.Fatal("文件类别映射错误", err)
			}
		})
	}
	for _, root := range []bool{false, true} {
		t.Run(fmt.Sprintf("directory/root=%t", root), func(t *testing.T) {
			result := entryDetailDataFixture()
			result.Path.Entries = result.Path.Entries[:2]
			if root {
				result.Path.Entries = result.Path.Entries[:1]
			}
			target := result.Path.Entries[len(result.Path.Entries)-1]
			result.Display = domain.FileEntryDisplay{ID: target.ID, UserID: target.UserID}
			result.Preference = domain.EntryPreference{UserID: target.UserID, EntryID: target.ID}
			data, err := NewEntryDetailData(result)
			if err != nil || data == nil || data.Kind != "folder" || data.SizeBytes != 0 || data.Starred || data.SourceDevice != nil {
				t.Fatal("目录类型、零大小或默认偏好错误", err)
			}
			encoded, err := json.Marshal(data)
			var payload map[string]any
			if err != nil || json.Unmarshal(encoded, &payload) != nil || len(payload) != 18 {
				t.Fatal("目录详情字段数量错误")
			}
			for _, field := range []string{"extension", "mime_type", "color_key", "content_modified_at", "source_device", "starred_at", "last_opened_at"} {
				value, exists := payload[field]
				if !exists || value != nil {
					t.Fatal("空字段未明确编码为 null", field)
				}
			}
			if root {
				if data.ParentID != nil || payload["parent_id"] != nil || data.Name != "" {
					t.Fatal("根目录父标识或名称错误")
				}
			} else if data.ParentID == nil || *data.ParentID != "11" {
				t.Fatal("普通目录父标识错误")
			}
		})
	}
	for _, mode := range []string{"id only", "name only", "empty name", "empty extension", "opened only", "starred only"} {
		t.Run(mode, func(t *testing.T) {
			result := entryDetailDataFixture()
			switch mode {
			case "id only":
				result.Display.SourceDeviceName = nil
			case "name only":
				result.Display.SourceDeviceID = nil
			case "empty name":
				*result.Display.SourceDeviceName = ""
			case "empty extension":
				*result.Display.Extension = ""
			case "opened only":
				result.Preference.StarredAt = nil
			case "starred only":
				result.Preference.LastOpenedAt = nil
			}
			data, err := NewEntryDetailData(result)
			if err != nil || data == nil || data.SourceDevice == nil || data.Starred != (result.Preference.StarredAt != nil) ||
				!reflect.DeepEqual(data.Extension, result.Display.Extension) || !reflect.DeepEqual(data.SourceDevice.Name, result.Display.SourceDeviceName) ||
				(data.SourceDevice.ID == nil) != (result.Display.SourceDeviceID == nil) || (data.LastOpenedAt == nil) != (result.Preference.LastOpenedAt == nil) {
				t.Fatal("历史来源、空文本或偏好空值转换错误", err)
			}
			encoded, err := json.Marshal(data.SourceDevice)
			var payload map[string]any
			if err != nil || json.Unmarshal(encoded, &payload) != nil || len(payload) != 2 || (payload["id"] == nil) != (result.Display.SourceDeviceID == nil) || (payload["name"] == nil) != (result.Display.SourceDeviceName == nil) {
				t.Fatal("来源未知字段未编码为明确 null")
			}
		})
	}
}

func TestNewEntryDetailDataBounds(t *testing.T) {
	t.Run("full identifiers revision and safe size", func(t *testing.T) {
		result := entryDetailDataFixture()
		maximum := ^uint64(0)
		for index := range result.Path.Entries {
			entry := &result.Path.Entries[index]
			entry.ID, entry.UserID = maximum-2+uint64(index), maximum
			if index > 0 {
				parent := result.Path.Entries[index-1].ID
				entry.ParentID = &parent
			}
		}
		result.Path.Entries[2].Revision, result.Path.Entries[2].FileSize = maximum, maxJSONSafeInteger
		result.Display.ID, result.Display.UserID, result.Display.SourceDeviceID = maximum, maximum, &maximum
		result.Preference.UserID, result.Preference.EntryID = maximum, maximum
		data, err := NewEntryDetailData(result)
		if err != nil || data == nil || data.ID != "18446744073709551615" || data.Revision != data.ID || *data.SourceDevice.ID != data.ID ||
			data.ParentID == nil || *data.ParentID != "18446744073709551614" || data.SizeBytes != maxJSONSafeInteger {
			t.Fatal("完整标识、版本或安全字节上限错误", err)
		}
	})
	t.Run("maximum path depth", func(t *testing.T) {
		result := entryDetailDataFixture()
		root := result.Path.Entries[0]
		result.Path.Entries = []domain.FileEntry{root}
		for depth := 1; depth <= domain.MaxFileEntryDepth; depth++ {
			parentID := result.Path.Entries[len(result.Path.Entries)-1].ID
			entry := root
			entry.ID, entry.ParentID, entry.Name = uint64(100+depth), &parentID, strconv.Itoa(depth)
			result.Path.Entries = append(result.Path.Entries, entry)
		}
		target := result.Path.Entries[len(result.Path.Entries)-1]
		color := "sand"
		result.Display = domain.FileEntryDisplay{ID: target.ID, UserID: target.UserID, ColorKey: &color}
		result.Preference = domain.EntryPreference{UserID: target.UserID, EntryID: target.ID}
		data, err := NewEntryDetailData(result)
		if err != nil || data == nil || data.ID != "132" || data.Kind != "folder" || data.ColorKey == nil || *data.ColorKey != color {
			t.Fatal("最大路径深度或目录颜色转换错误", err)
		}
	})
	for _, stamp := range []time.Time{time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)} {
		result := entryDetailDataFixture()
		target := &result.Path.Entries[2]
		target.CreatedAt, target.ModifiedAt, target.UpdatedAt = stamp, stamp, stamp
		result.Display.ContentModifiedAt, result.Preference.StarredAt, result.Preference.LastOpenedAt = &stamp, &stamp, &stamp
		data, err := NewEntryDetailData(result)
		if err != nil || data == nil || !data.CreatedAt.Equal(stamp) || !data.ContentModifiedAt.Equal(stamp) || !data.StarredAt.Equal(stamp) || !data.LastOpenedAt.Equal(stamp) {
			t.Fatal("数据库日期边界错误", err)
		}
	}
}

func TestNewEntryDetailDataFailures(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		change       func(*domain.EntryDetail)
		numericError bool
	}{
		{name: "empty path", change: func(result *domain.EntryDetail) { result.Path.Entries = nil }},
		{name: "oversize path", change: func(result *domain.EntryDetail) { result.Path.Entries = make([]domain.FileEntry, 34) }},
		{name: "zero owner", change: func(result *domain.EntryDetail) { result.Path.Entries[0].UserID = 0 }},
		{name: "zero target", change: func(result *domain.EntryDetail) { result.Path.Entries[2].ID = 0 }},
		{name: "foreign ancestor", change: func(result *domain.EntryDetail) { result.Path.Entries[1].UserID = 8 }},
		{name: "foreign target", change: func(result *domain.EntryDetail) { result.Path.Entries[2].UserID = 8 }},
		{name: "hidden target", change: func(result *domain.EntryDetail) { result.Path.Entries[2].Status = "TRASHED" }},
		{name: "hidden ancestor", change: func(result *domain.EntryDetail) { result.Path.Entries[1].Status = "PURGING" }},
		{name: "purged target", change: func(result *domain.EntryDetail) { result.Path.Entries[2].Status = "PURGED" }},
		{name: "unknown type", change: func(result *domain.EntryDetail) { result.Path.Entries[2].EntryType = "UNKNOWN" }},
		{name: "file ancestor", change: func(result *domain.EntryDetail) { result.Path.Entries[1].EntryType = "FILE" }},
		{name: "directory size", change: func(result *domain.EntryDetail) { result.Path.Entries[1].FileSize = 1 }},
		{name: "zero revision", change: func(result *domain.EntryDetail) { result.Path.Entries[2].Revision = 0 }},
		{name: "named root", change: func(result *domain.EntryDetail) { result.Path.Entries[0].Name = "伪根目录" }},
		{name: "root parent", change: func(result *domain.EntryDetail) { parent := uint64(1); result.Path.Entries[0].ParentID = &parent }},
		{name: "missing parent", change: func(result *domain.EntryDetail) { result.Path.Entries[2].ParentID = nil }},
		{name: "wrong parent", change: func(result *domain.EntryDetail) { *result.Path.Entries[2].ParentID = 99 }},
		{name: "empty ordinary name", change: func(result *domain.EntryDetail) { result.Path.Entries[2].Name = "" }},
		{name: "duplicate identifier", change: func(result *domain.EntryDetail) { result.Path.Entries[2].ID = 11 }},
		{name: "display owner", change: func(result *domain.EntryDetail) { result.Display.UserID = 8 }},
		{name: "display target", change: func(result *domain.EntryDetail) { result.Display.ID = 24 }},
		{name: "preference owner", change: func(result *domain.EntryDetail) { result.Preference.UserID = 8 }},
		{name: "preference target", change: func(result *domain.EntryDetail) { result.Preference.EntryID = 24 }},
		{name: "zero source", change: func(result *domain.EntryDetail) { *result.Display.SourceDeviceID = 0 }},
		{name: "missing category", change: func(result *domain.EntryDetail) { result.Display.Category = nil }},
		{name: "unknown category", change: func(result *domain.EntryDetail) { *result.Display.Category = "UNKNOWN" }},
		{name: "lowercase category", change: func(result *domain.EntryDetail) { *result.Display.Category = "document" }},
		{name: "directory category", change: func(result *domain.EntryDetail) {
			result.Path.Entries[2].EntryType, result.Path.Entries[2].FileSize = "DIR", 0
		}},
		{name: "unsafe size", change: func(result *domain.EntryDetail) { result.Path.Entries[2].FileSize = maxJSONSafeInteger + 1 }, numericError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := entryDetailDataFixture()
			testCase.change(result)
			data, err := NewEntryDetailData(result)
			if data != nil || err == nil || (!testCase.numericError && !errors.Is(err, domain.ErrInvalidFileEntryData)) {
				t.Fatal("异常详情未返回空结果及适用错误", err)
			}
		})
	}
	if data, err := NewEntryDetailData(nil); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
		t.Fatal("空详情未拒绝", err)
	}
	for _, field := range []string{"created", "modified", "updated", "content", "starred", "opened"} {
		for _, stamp := range []time.Time{time.Time{}, time.Date(999, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
			result := entryDetailDataFixture()
			switch field {
			case "created":
				result.Path.Entries[2].CreatedAt = stamp
			case "modified":
				result.Path.Entries[2].ModifiedAt = stamp
			case "updated":
				result.Path.Entries[2].UpdatedAt = stamp
			case "content":
				result.Display.ContentModifiedAt = &stamp
			case "starred":
				result.Preference.StarredAt = &stamp
			case "opened":
				result.Preference.LastOpenedAt = &stamp
			}
			if data, err := NewEntryDetailData(result); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("非法详情日期仍完成转换", field, err)
			}
		}
	}
}

func TestNewEntryDetailDataCopies(t *testing.T) {
	result := entryDetailDataFixture()
	color := "green"
	result.Display.ColorKey = &color
	inputBefore, _ := json.Marshal(result)
	first, err := NewEntryDetailData(result)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewEntryDetailData(result)
	inputAfter, _ := json.Marshal(result)
	if err != nil || !reflect.DeepEqual(first, second) || !bytes.Equal(inputBefore, inputAfter) {
		t.Fatal("转换修改了来源或重复结果不同", err)
	}
	baseline, _ := json.Marshal(second)
	*result.Path.Entries[2].ParentID, *result.Display.SourceDeviceID = 99, 99
	*result.Display.Extension, *result.Display.MIMEType, *result.Display.ColorKey, *result.Display.SourceDeviceName = "changed", "changed", "changed", "changed"
	*result.Display.ContentModifiedAt, *result.Preference.StarredAt, *result.Preference.LastOpenedAt = time.Time{}, time.Time{}, time.Time{}
	encoded, _ := json.Marshal(first)
	if !bytes.Equal(encoded, baseline) {
		t.Fatal("修改内部来源影响了已转换字段")
	}
	*first.ParentID, *first.SourceDevice.ID, *first.SourceDevice.Name = "88", "88", "local"
	*first.Extension, *first.MIMEType, *first.ColorKey = "local", "local", "local"
	*first.ContentModifiedAt, *first.StarredAt, *first.LastOpenedAt = time.Time{}, time.Time{}, time.Time{}
	encoded, _ = json.Marshal(second)
	if !bytes.Equal(encoded, baseline) {
		t.Fatal("两次详情 DTO 共享可修改字段")
	}
}
