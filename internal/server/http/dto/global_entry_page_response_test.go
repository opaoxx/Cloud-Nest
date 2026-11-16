package dto

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func globalEntryPageDataFixture() *domain.GlobalEntryPageResult {
	file := directoryPageDataFixture().Entries[0]
	directory := file
	parentID := uint64(14)
	directory.ID, directory.ParentID, directory.Name, directory.EntryType, directory.FileSize, directory.Revision = 24, &parentID, "跨目录文件夹", "DIR", 0, 1
	return &domain.GlobalEntryPageResult{Entries: []domain.FileEntry{file, directory}}
}

func TestNewGlobalEntryPageDataJSON(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, mode := range []string{"entries end", "entries next", "empty end", "empty next"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 转换跨目录结果，核对精确 JSON 与公开字段。
			result := globalEntryPageDataFixture()
			if strings.HasPrefix(mode, "empty") {
				result.Entries = nil
			}
			if strings.HasSuffix(mode, "next") {
				cursor := "fixture-global-cursor"
				result.NextCursor = &cursor
			}
			data, err := NewGlobalEntryPageData(result)
			if err != nil || data == nil || data.Items == nil || data.Breadcrumbs == nil || len(data.Breadcrumbs) != 0 {
				t.Fatal("全局 DTO 或空切片错误", err)
			}
			encoded, err := json.Marshal(GlobalEntryPageResponse{Data: *data, RequestID: requestID})
			if err != nil {
				t.Fatal(err)
			}
			items := `[]`
			if len(result.Entries) != 0 {
				items = `[{"id":"23","parent_id":"12","name":"学习笔记.md","entry_type":"FILE","size_bytes":2048,"revision":"3","created_at":"2026-11-15T01:30:00.123456Z","modified_at":"2026-11-15T02:30:00.123456Z","updated_at":"2026-11-15T02:30:00.123456Z","status":"ACTIVE"},{"id":"24","parent_id":"14","name":"跨目录文件夹","entry_type":"DIR","size_bytes":0,"revision":"1","created_at":"2026-11-15T01:30:00.123456Z","modified_at":"2026-11-15T02:30:00.123456Z","updated_at":"2026-11-15T02:30:00.123456Z","status":"ACTIVE"}]`
			}
			cursorJSON := "null"
			if result.NextCursor != nil {
				cursorJSON = `"fixture-global-cursor"`
			}
			want := `{"data":{"items":` + items + `,"breadcrumbs":[],"next_cursor":` + cursorJSON + `},"request_id":"` + requestID + `"}`
			if string(encoded) != want || strings.Contains(string(encoded), "user_id") {
				t.Fatal("全局 JSON 字段、时间或游标值错误", string(encoded))
			}
		})
	}
}

func TestNewGlobalEntryPageDataBounds(t *testing.T) {
	for _, mode := range []string{"maximum identifiers", "maximum size", "zero size", "maximum page", "maximum cursor", "minimum time", "maximum time"} {
		t.Run(mode, func(t *testing.T) {
			result := globalEntryPageDataFixture()
			switch mode {
			case "maximum identifiers":
				maximum := ^uint64(0)
				for index := range result.Entries {
					parentID := maximum - 2
					result.Entries[index].ID, result.Entries[index].UserID, result.Entries[index].Revision, result.Entries[index].ParentID = maximum-uint64(index), maximum, maximum, &parentID
				}
			case "maximum size":
				result.Entries[0].FileSize = maxJSONSafeInteger
			case "zero size":
				result.Entries[0].FileSize = 0
			case "maximum page":
				file := result.Entries[0]
				result.Entries = nil
				for index := range 100 {
					file.ID = uint64(100 + index)
					result.Entries = append(result.Entries, file)
				}
			case "maximum cursor":
				cursor := strings.Repeat("x", domain.MaxGlobalEntryCursorLength)
				result.NextCursor = &cursor
			case "minimum time", "maximum time":
				stamp := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				if mode == "maximum time" {
					stamp = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
				}
				for index := range result.Entries {
					result.Entries[index].CreatedAt, result.Entries[index].ModifiedAt, result.Entries[index].UpdatedAt = stamp, stamp, stamp
				}
			}
			data, err := NewGlobalEntryPageData(result)
			if err != nil || data == nil || len(data.Items) != len(result.Entries) {
				t.Fatal("合法全局 DTO 边界被拒绝", err)
			}
			for index, item := range data.Items {
				entry := result.Entries[index]
				if item.SizeBytes != entry.FileSize || item.CreatedAt.Location() != time.UTC || item.CreatedAt.Nanosecond()%1000 != 0 || !item.CreatedAt.Equal(entry.CreatedAt.UTC().Truncate(time.Microsecond)) {
					t.Fatal("边界字段或微秒值不准确")
				}
				if mode == "maximum identifiers" && (item.Revision != "18446744073709551615" || item.ParentID != "18446744073709551613" || (index == 0 && item.ID != "18446744073709551615")) {
					t.Fatal("完整无符号标识被截断")
				}
			}
		})
	}
}

func TestNewGlobalEntryPageDataFailures(t *testing.T) {
	for _, mode := range []string{"nil result", "large page", "empty cursor", "large cursor", "zero owner", "mixed owners", "zero id", "nil parent", "zero parent", "zero revision", "empty name", "inactive", "bad type", "directory size", "unsafe size", "bad later item"} {
		t.Run(mode, func(t *testing.T) {
			result := globalEntryPageDataFixture()
			switch mode {
			case "nil result":
				result = nil
			case "large page":
				result.Entries = make([]domain.FileEntry, 101)
			case "empty cursor":
				cursor := ""
				result.NextCursor = &cursor
			case "large cursor":
				cursor := strings.Repeat("x", domain.MaxGlobalEntryCursorLength+1)
				result.NextCursor = &cursor
			case "zero owner":
				result.Entries[0].UserID = 0
			case "mixed owners":
				result.Entries[1].UserID = 8
			case "zero id":
				result.Entries[0].ID = 0
			case "nil parent":
				result.Entries[0].ParentID = nil
			case "zero parent":
				result.Entries[0].ParentID = new(uint64)
			case "zero revision":
				result.Entries[0].Revision = 0
			case "empty name":
				result.Entries[0].Name = ""
			case "inactive":
				result.Entries[0].Status = "TRASHED"
			case "bad type":
				result.Entries[0].EntryType = "LINK"
			case "directory size":
				result.Entries[1].FileSize = 1
			case "unsafe size":
				result.Entries[0].FileSize = maxJSONSafeInteger + 1
			case "bad later item":
				result.Entries[1].ModifiedAt = time.Time{}
			}
			data, err := NewGlobalEntryPageData(result)
			if data != nil || err == nil || (mode != "unsafe size" && !errors.Is(err, domain.ErrInvalidFileEntryData)) {
				t.Fatal("全局异常结果被接受或返回部分数据", err)
			}
		})
	}
	for _, field := range []string{"created", "modified", "updated"} {
		for _, stamp := range []time.Time{time.Time{}, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1000, 1, 1, 0, 0, 0, 0, time.FixedZone("fixture", 3600))} {
			result := globalEntryPageDataFixture()
			switch field {
			case "created":
				result.Entries[0].CreatedAt = stamp
			case "modified":
				result.Entries[0].ModifiedAt = stamp
			case "updated":
				result.Entries[0].UpdatedAt = stamp
			}
			if data, err := NewGlobalEntryPageData(result); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常 UTC 日期被接受", field, err)
			}
		}
	}
}

func TestNewGlobalEntryPageDataCopies(t *testing.T) {
	result := globalEntryPageDataFixture()
	cursor := "fixture-next"
	result.NextCursor = &cursor
	original := *result.Entries[0].ParentID
	first, err := NewGlobalEntryPageData(result)
	if err != nil {
		t.Fatal(err)
	}
	expected := append([]DirectoryEntrySummary(nil), first.Items...)
	first.Items[0].Name = "本地修改"
	*first.NextCursor = "local-next"
	first.Breadcrumbs = append(first.Breadcrumbs, DirectoryBreadcrumb{ID: "999", Name: "本地"})
	second, err := NewGlobalEntryPageData(result)
	if err != nil || second.Items[0].Name != "学习笔记.md" || *second.NextCursor != cursor || len(second.Breadcrumbs) != 0 || result.Entries[0].Name != "学习笔记.md" || *result.Entries[0].ParentID != original || first.NextCursor == second.NextCursor || first.NextCursor == result.NextCursor {
		t.Fatal("全局 DTO 副本与源值互相影响", err)
	}
	if !reflect.DeepEqual(second.Items, expected) {
		t.Fatal("重复转换值不稳定")
	}
}
