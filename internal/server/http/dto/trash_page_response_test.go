package dto

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func trashPageDataFixture() *domain.TrashEntryPageResult {
	entry := trashEntryDataFixture()
	root := domain.FileEntry{ID: 11, UserID: entry.UserID, EntryType: "DIR", Status: "ACTIVE"}
	parent := domain.FileEntry{ID: 12, UserID: entry.UserID, ParentID: new(uint64(11)), Name: "学习资料", EntryType: "DIR", Status: "ACTIVE"}
	return &domain.TrashEntryPageResult{
		Entries: []domain.TrashEntryListItem{{
			Entry:        *entry,
			OriginalPath: domain.FileEntryPath{Entries: []domain.FileEntry{root, parent}},
		}},
	}
}

func TestNewTrashPageDataJSON(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const file = `{"id":"23","parent_id":"12","name":"学习笔记.md","entry_type":"FILE","status":"TRASHED","revision":"4","deleted_at":"2026-11-20T04:30:00.123456Z","size_bytes":2048,"original_path":[{"id":"11","name":""},{"id":"12","name":"学习资料"}],"retention_expires_at":"2026-12-20T04:30:00.123456Z"}`
	for _, testCase := range []struct {
		name    string
		empty   bool
		hasMore bool
	}{
		{name: "last page with file"},
		{name: "file with cursor", hasMore: true},
		{name: "empty final page", empty: true},
		{name: "empty budget page", empty: true, hasMore: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 逐字段核对公开 JSON，确认空数组、可空游标和请求 ID。
			result := trashPageDataFixture()
			items, cursor := "["+file+"]", "null"
			if testCase.empty {
				result.Entries = nil
				items = "[]"
			}
			if testCase.hasMore {
				result.NextCursor = new("fixture-cursor")
				cursor = `"fixture-cursor"`
			}
			data, err := NewTrashPageData(result)
			if err != nil || data == nil {
				t.Fatal("回收站列表转换失败", err)
			}
			got, err := json.Marshal(TrashPageResponse{Data: *data, RequestID: requestID})
			want := `{"data":{"items":` + items + `,"next_cursor":` + cursor + `},"request_id":"` + requestID + `"}`
			if err != nil || string(got) != want {
				t.Fatalf("响应为 %s，期望 %s，错误为 %v", got, want, err)
			}

			// 2. 内部记录和游标后续改变时，公开数据仍保留原值。
			if !testCase.empty {
				*result.Entries[0].Entry.ParentID = 99
				result.Entries[0].OriginalPath.Entries[1].Name = "稍后改名"
				if data.Items[0].ParentID != "12" || data.Items[0].OriginalPath[1].Name != "学习资料" {
					t.Fatal("原父路径仍依赖输入对象")
				}
			}
			if testCase.hasMore {
				*result.NextCursor = "changed"
				if *data.NextCursor != "fixture-cursor" {
					t.Fatal("公开游标仍依赖输入指针")
				}
			}
		})
	}
}

func TestNewTrashPageDataBounds(t *testing.T) {
	t.Run("directory with maximum identifiers and cursor", func(t *testing.T) {
		result := trashPageDataFixture()
		item := &result.Entries[0]
		path := item.OriginalPath.Entries
		path[1].ID = math.MaxUint64 - 1
		item.Entry.ID, item.Entry.Revision, *item.Entry.ParentID = math.MaxUint64, math.MaxUint64, path[1].ID
		item.Entry.EntryType, item.Entry.FileSize = "DIR", 0
		result.NextCursor = new(strings.Repeat("x", domain.MaxTrashEntryCursorLength))
		data, err := NewTrashPageData(result)
		if err != nil || data == nil || data.Items[0].ID != "18446744073709551615" ||
			data.Items[0].Revision != "18446744073709551615" || data.Items[0].ParentID != "18446744073709551614" ||
			data.Items[0].SizeBytes != 0 || data.NextCursor == nil || *data.NextCursor != *result.NextCursor {
			t.Fatalf("目录边界结果为 %+v，错误为 %v", data, err)
		}
	})
	t.Run("maximum size and path depth", func(t *testing.T) {
		result := trashPageDataFixture()
		item := &result.Entries[0]
		item.Entry.FileSize = maxJSONSafeInteger
		path := item.OriginalPath.Entries
		for len(path) < domain.MaxFileEntryDepth {
			parent := path[len(path)-1]
			path = append(path, domain.FileEntry{
				ID: uint64(100 + len(path)), UserID: parent.UserID, ParentID: new(parent.ID),
				Name: "下层目录", EntryType: "DIR", Status: "ACTIVE",
			})
		}
		item.OriginalPath.Entries = path
		*item.Entry.ParentID = path[len(path)-1].ID
		data, err := NewTrashPageData(result)
		if err != nil || data == nil || data.Items[0].SizeBytes != maxJSONSafeInteger ||
			len(data.Items[0].OriginalPath) != domain.MaxFileEntryDepth {
			t.Fatalf("字节数或目录深度的边界结果为 %+v，错误为 %v", data, err)
		}
	})
	t.Run("last representable retention expiry", func(t *testing.T) {
		result := trashPageDataFixture()
		result.Entries[0].Entry.DeletedAt = time.Date(9999, 12, 1, 23, 59, 59, 999999000, time.UTC)
		data, err := NewTrashPageData(result)
		if err != nil || data == nil || data.Items[0].RetentionExpiresAt != time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC) {
			t.Fatalf("保留时间边界结果为 %+v，错误为 %v", data, err)
		}
	})
}

func TestNewTrashPageDataRejectsInvalidResult(t *testing.T) {
	if data, err := NewTrashPageData(nil); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
		t.Fatalf("空对象转换结果为 %+v，错误为 %v", data, err)
	}
	for _, testCase := range []struct {
		name string
		edit func(*domain.TrashEntryPageResult)
	}{
		{"too many entries", func(result *domain.TrashEntryPageResult) {
			result.Entries = make([]domain.TrashEntryListItem, 101)
		}},
		{"empty cursor", func(result *domain.TrashEntryPageResult) { result.NextCursor = new("") }},
		{"long cursor", func(result *domain.TrashEntryPageResult) {
			result.NextCursor = new(strings.Repeat("a", domain.MaxTrashEntryCursorLength+1))
		}},
		{"invalid cursor UTF-8", func(result *domain.TrashEntryPageResult) { result.NextCursor = new("\xff") }},
		{"unsafe file size", func(result *domain.TrashEntryPageResult) { result.Entries[0].Entry.FileSize = maxJSONSafeInteger + 1 }},
		{"wrong entry state", func(result *domain.TrashEntryPageResult) { result.Entries[0].Entry.Status = "ACTIVE" }},
		{"absent original path", func(result *domain.TrashEntryPageResult) { result.Entries[0].OriginalPath.Entries = nil }},
		{"oversize original path", func(result *domain.TrashEntryPageResult) {
			result.Entries[0].OriginalPath.Entries = make([]domain.FileEntry, domain.MaxFileEntryDepth+1)
		}},
		{"path does not end at parent", func(result *domain.TrashEntryPageResult) { *result.Entries[0].Entry.ParentID = 95 }},
		{"foreign directory", func(result *domain.TrashEntryPageResult) { result.Entries[0].OriginalPath.Entries[1].UserID++ }},
		{"inactive directory", func(result *domain.TrashEntryPageResult) {
			result.Entries[0].OriginalPath.Entries[1].Status = "PURGING"
		}},
		{"non-directory ancestor", func(result *domain.TrashEntryPageResult) {
			result.Entries[0].OriginalPath.Entries[1].EntryType = "FILE"
		}},
		{"named root", func(result *domain.TrashEntryPageResult) {
			result.Entries[0].OriginalPath.Entries[0].Name = "假根目录"
		}},
		{"broken chain", func(result *domain.TrashEntryPageResult) { *result.Entries[0].OriginalPath.Entries[1].ParentID = 95 }},
		{"repeated path ID", func(result *domain.TrashEntryPageResult) {
			result.Entries[0].OriginalPath.Entries[0].ID = 12
			*result.Entries[0].OriginalPath.Entries[1].ParentID = 12
		}},
		{"expiry outside four-digit year", func(result *domain.TrashEntryPageResult) {
			result.Entries[0].Entry.DeletedAt = time.Date(9999, 12, 2, 0, 0, 0, 0, time.UTC)
		}},
		{"different roots for same user", func(result *domain.TrashEntryPageResult) {
			other := trashPageDataFixture().Entries[0]
			other.Entry.ID++
			other.OriginalPath.Entries[0].ID = 50
			*other.OriginalPath.Entries[1].ParentID = 50
			result.Entries = append(result.Entries, other)
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 异常数据拒绝整页输出。
			result := trashPageDataFixture()
			testCase.edit(result)
			if data, err := NewTrashPageData(result); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatalf("转换结果为 %+v，错误为 %v", data, err)
			}
		})
	}
}
