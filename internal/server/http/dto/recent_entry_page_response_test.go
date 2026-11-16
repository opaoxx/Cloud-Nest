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

func recentEntryPageDataFixture() *domain.RecentEntryPageResult {
	file := directoryPageDataFixture().Entries[0]
	other := file
	parentID := uint64(14)
	other.ID, other.ParentID, other.Name = 24, &parentID, "跨目录计划.md"
	openedAt := file.ModifiedAt.Add(24 * time.Hour)
	return &domain.RecentEntryPageResult{Entries: []domain.RecentEntryCandidate{
		{Entry: file, LastOpenedAt: openedAt},
		{Entry: other, LastOpenedAt: openedAt.Add(-time.Hour)},
	}}
}

func TestNewRecentEntryPageDataJSON(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, mode := range []string{"entries end", "entries next", "empty end", "empty next"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 转换跨目录最近文件，核对平铺字段和精确 JSON。
			result := recentEntryPageDataFixture()
			if strings.HasPrefix(mode, "empty") {
				result.Entries = nil
			}
			if strings.HasSuffix(mode, "next") {
				cursor := "fixture-recent-cursor"
				result.NextCursor = &cursor
			}
			data, err := NewRecentEntryPageData(result)
			if err != nil || data == nil || data.Items == nil || data.Breadcrumbs == nil || len(data.Breadcrumbs) != 0 {
				t.Fatal("最近分页 DTO 或空数组错误", err)
			}
			encoded, err := json.Marshal(RecentEntryPageResponse{Data: *data, RequestID: requestID})
			if err != nil {
				t.Fatal(err)
			}
			items := `[]`
			if len(result.Entries) != 0 {
				items = `[{"id":"23","parent_id":"12","name":"学习笔记.md","entry_type":"FILE","size_bytes":2048,"revision":"3","created_at":"2026-11-15T01:30:00.123456Z","modified_at":"2026-11-15T02:30:00.123456Z","updated_at":"2026-11-15T02:30:00.123456Z","status":"ACTIVE","last_opened_at":"2026-11-16T02:30:00.123456Z"},{"id":"24","parent_id":"14","name":"跨目录计划.md","entry_type":"FILE","size_bytes":2048,"revision":"3","created_at":"2026-11-15T01:30:00.123456Z","modified_at":"2026-11-15T02:30:00.123456Z","updated_at":"2026-11-15T02:30:00.123456Z","status":"ACTIVE","last_opened_at":"2026-11-16T01:30:00.123456Z"}]`
			}
			cursorJSON := "null"
			if result.NextCursor != nil {
				cursorJSON = `"fixture-recent-cursor"`
			}
			want := `{"data":{"items":` + items + `,"breadcrumbs":[],"next_cursor":` + cursorJSON + `},"request_id":"` + requestID + `"}`
			if string(encoded) != want || strings.Contains(string(encoded), "user_id") {
				t.Fatal("最近分页字段、访问时间或游标错误", string(encoded))
			}
		})
	}
}

func TestNewRecentEntryPageDataBounds(t *testing.T) {
	for _, mode := range []string{"maximum identifiers", "maximum size", "zero size", "maximum page", "maximum cursor", "minimum time", "maximum time", "local year normalizes into range", "submicrosecond time"} {
		t.Run(mode, func(t *testing.T) {
			result := recentEntryPageDataFixture()
			switch mode {
			case "maximum identifiers":
				maximum := ^uint64(0)
				for index := range result.Entries {
					parentID := maximum - 2
					entry := &result.Entries[index].Entry
					entry.ID, entry.UserID, entry.Revision, entry.ParentID = maximum-uint64(index), maximum, maximum, &parentID
				}
			case "maximum size":
				result.Entries[0].Entry.FileSize = maxJSONSafeInteger
			case "zero size":
				result.Entries[0].Entry.FileSize = 0
			case "maximum page":
				candidate := result.Entries[0]
				result.Entries = nil
				for index := range 100 {
					candidate.Entry.ID = uint64(100 + index)
					result.Entries = append(result.Entries, candidate)
				}
			case "maximum cursor":
				cursor := strings.Repeat("x", domain.MaxRecentEntryCursorLength)
				result.NextCursor = &cursor
			case "minimum time", "maximum time", "local year normalizes into range", "submicrosecond time":
				stamp := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				switch mode {
				case "maximum time":
					stamp = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
				case "local year normalizes into range":
					stamp = time.Date(10000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 3600))
				case "submicrosecond time":
					stamp = time.Date(2026, 11, 16, 2, 30, 0, 999, time.UTC)
				}
				for index := range result.Entries {
					result.Entries[index].LastOpenedAt = stamp
				}
			}
			originalTime := result.Entries[0].LastOpenedAt
			data, err := NewRecentEntryPageData(result)
			if err != nil || data == nil || len(data.Items) != len(result.Entries) || result.Entries[0].LastOpenedAt != originalTime {
				t.Fatal("合法最近分页边界或输入时间错误", err)
			}
			for index, item := range data.Items {
				candidate := result.Entries[index]
				if item.SizeBytes != candidate.Entry.FileSize || item.CreatedAt.Location() != time.UTC || item.CreatedAt.Nanosecond()%1000 != 0 || !item.CreatedAt.Equal(candidate.Entry.CreatedAt.UTC().Truncate(time.Microsecond)) || item.LastOpenedAt.Location() != time.UTC || item.LastOpenedAt.Nanosecond()%1000 != 0 || !item.LastOpenedAt.Equal(candidate.LastOpenedAt.UTC().Truncate(time.Microsecond)) {
					t.Fatal("最近分页边界字段或 UTC 微秒值不准确")
				}
				if mode == "maximum identifiers" && (item.Revision != "18446744073709551615" || item.ParentID != "18446744073709551613" || (index == 0 && item.ID != "18446744073709551615")) {
					t.Fatal("最近文件完整无符号标识被截断")
				}
			}
			if _, err := json.Marshal(data); err != nil {
				t.Fatal("最近分页边界数据无法编码", err)
			}
		})
	}
}

func TestNewRecentEntryPageDataFailures(t *testing.T) {
	for _, mode := range []string{"nil result", "large page", "empty cursor", "large cursor", "zero owner", "mixed owners", "zero id", "nil parent", "zero parent", "zero revision", "empty name", "inactive", "bad type", "directory", "unsafe size", "bad later item", "bad later access"} {
		t.Run(mode, func(t *testing.T) {
			result := recentEntryPageDataFixture()
			switch mode {
			case "nil result":
				result = nil
			case "large page":
				result.Entries = make([]domain.RecentEntryCandidate, 101)
			case "empty cursor":
				cursor := ""
				result.NextCursor = &cursor
			case "large cursor":
				cursor := strings.Repeat("x", domain.MaxRecentEntryCursorLength+1)
				result.NextCursor = &cursor
			case "zero owner":
				result.Entries[0].Entry.UserID = 0
			case "mixed owners":
				result.Entries[1].Entry.UserID = 8
			case "zero id":
				result.Entries[0].Entry.ID = 0
			case "nil parent":
				result.Entries[0].Entry.ParentID = nil
			case "zero parent":
				result.Entries[0].Entry.ParentID = new(uint64)
			case "zero revision":
				result.Entries[0].Entry.Revision = 0
			case "empty name":
				result.Entries[0].Entry.Name = ""
			case "inactive":
				result.Entries[0].Entry.Status = "TRASHED"
			case "bad type":
				result.Entries[0].Entry.EntryType = "LINK"
			case "directory":
				result.Entries[0].Entry.EntryType, result.Entries[0].Entry.FileSize = "DIR", 0
			case "unsafe size":
				result.Entries[0].Entry.FileSize = maxJSONSafeInteger + 1
			case "bad later item":
				result.Entries[1].Entry.ModifiedAt = time.Time{}
			case "bad later access":
				result.Entries[1].LastOpenedAt = time.Time{}
			}
			if data, err := NewRecentEntryPageData(result); data != nil || err == nil || (mode != "unsafe size" && !errors.Is(err, domain.ErrInvalidFileEntryData)) {
				t.Fatal("异常最近分页结果被接受或交付部分数据", err)
			}
		})
	}
	for _, field := range []string{"created", "modified", "updated", "opened"} {
		for _, stamp := range []time.Time{time.Time{}, time.Date(999, 12, 31, 23, 59, 59, 999999999, time.UTC), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 3600)), time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600))} {
			result := recentEntryPageDataFixture()
			switch field {
			case "created":
				result.Entries[0].Entry.CreatedAt = stamp
			case "modified":
				result.Entries[0].Entry.ModifiedAt = stamp
			case "updated":
				result.Entries[0].Entry.UpdatedAt = stamp
			case "opened":
				result.Entries[0].LastOpenedAt = stamp
			}
			if data, err := NewRecentEntryPageData(result); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("最近分页非法 UTC 日期被接受", field, err)
			}
		}
	}
}

func TestNewRecentEntryPageDataCopies(t *testing.T) {
	result := recentEntryPageDataFixture()
	cursor := "fixture-next"
	result.NextCursor = &cursor
	expectedCursor := cursor
	original := result.Entries[0]
	first, err := NewRecentEntryPageData(result)
	if err != nil {
		t.Fatal(err)
	}
	expected := append([]RecentEntrySummary(nil), first.Items...)
	first.Items[0].Name = "本地修改"
	first.Items[0].LastOpenedAt = time.Time{}
	*first.NextCursor = "local-next"
	first.Breadcrumbs = append(first.Breadcrumbs, DirectoryBreadcrumb{ID: "999", Name: "本地"})
	second, err := NewRecentEntryPageData(result)
	if err != nil || second == nil || !reflect.DeepEqual(second.Items, expected) || *second.NextCursor != expectedCursor || len(second.Breadcrumbs) != 0 || !reflect.DeepEqual(result.Entries[0], original) || first.NextCursor == second.NextCursor || first.NextCursor == result.NextCursor {
		t.Fatal("最近分页 DTO 与来源或其他结果互相影响", err)
	}
	result.Entries[0].Entry.Name = "来源后续修改"
	result.Entries[0].LastOpenedAt = time.Time{}
	*result.NextCursor = "source-next"
	if !reflect.DeepEqual(second.Items, expected) || *second.NextCursor != expectedCursor {
		t.Fatal("来源修改影响已有最近分页响应")
	}
}
