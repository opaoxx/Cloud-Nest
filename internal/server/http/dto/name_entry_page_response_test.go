package dto

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func nameEntryPageDataFixture() *domain.NameEntryPageResult {
	listed := globalEntryPageDataFixture()
	return &domain.NameEntryPageResult{Entries: []domain.NameEntryCandidate{
		{Entry: listed.Entries[0], NameKey: []byte{'a', 0, 0xff}},
		{Entry: listed.Entries[1], NameKey: []byte{'b', 0, 0xff}},
	}}
}

func TestNewNameEntryPageDataJSON(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, mode := range []string{"entries end", "entries next", "nil empty end", "nil empty next", "initialized empty end", "initialized empty next"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 转换文件和目录，核对十字段摘要及各类空页的精确 JSON。
			result := nameEntryPageDataFixture()
			if strings.Contains(mode, "empty") {
				result.Entries = nil
			}
			if strings.HasPrefix(mode, "initialized") {
				result.Entries = make([]domain.NameEntryCandidate, 0)
			}
			if strings.HasSuffix(mode, "next") {
				cursor := "fixture-name-cursor"
				result.NextCursor = &cursor
			}
			data, err := NewNameEntryPageData(result)
			if err != nil || data == nil || data.Items == nil || data.Breadcrumbs == nil || len(data.Breadcrumbs) != 0 {
				t.Fatal("名称分页 DTO 或空数组错误", err)
			}
			encoded, err := json.Marshal(NameEntryPageResponse{Data: *data, RequestID: requestID})
			if err != nil {
				t.Fatal(err)
			}
			items := `[]`
			if len(result.Entries) != 0 {
				items = `[{"id":"23","parent_id":"12","name":"学习笔记.md","entry_type":"FILE","size_bytes":2048,"revision":"3","created_at":"2026-11-15T01:30:00.123456Z","modified_at":"2026-11-15T02:30:00.123456Z","updated_at":"2026-11-15T02:30:00.123456Z","status":"ACTIVE"},{"id":"24","parent_id":"14","name":"跨目录文件夹","entry_type":"DIR","size_bytes":0,"revision":"1","created_at":"2026-11-15T01:30:00.123456Z","modified_at":"2026-11-15T02:30:00.123456Z","updated_at":"2026-11-15T02:30:00.123456Z","status":"ACTIVE"}]`
			}
			cursorJSON := "null"
			if result.NextCursor != nil {
				cursorJSON = `"fixture-name-cursor"`
			}
			want := `{"data":{"items":` + items + `,"breadcrumbs":[],"next_cursor":` + cursorJSON + `},"request_id":"` + requestID + `"}`
			if string(encoded) != want {
				t.Fatal("名称分页 JSON 字段、时间或游标错误", string(encoded))
			}
		})
	}
}

func TestNewNameEntryPageDataBounds(t *testing.T) {
	for _, mode := range []string{
		"maximum identifiers", "maximum size", "zero size", "maximum page", "long cursor", "maximum cursor", "maximum UTF-8 cursor",
		"minimum time", "maximum time", "local year normalizes into range", "submicrosecond time", "binary keys", "display and order",
	} {
		t.Run(mode, func(t *testing.T) {
			// 1. 沿用公共摘要边界，补充名称文本容量及原始键隔离。
			result := nameEntryPageDataFixture()
			switch mode {
			case "maximum identifiers":
				maximum := ^uint64(0)
				for index := range result.Entries {
					entry := &result.Entries[index].Entry
					parentID := maximum - 2
					entry.ID, entry.UserID, entry.Revision, entry.ParentID = maximum-uint64(index), maximum, maximum, &parentID
				}
			case "maximum size":
				result.Entries[0].Entry.FileSize = maxJSONSafeInteger
			case "zero size":
				result.Entries[0].Entry.FileSize = 0
			case "maximum page":
				candidate := result.Entries[0]
				result.Entries = make([]domain.NameEntryCandidate, 100)
				for index := range result.Entries {
					candidate.Entry.ID = uint64(100 + index)
					result.Entries[index] = candidate
				}
			case "long cursor", "maximum cursor", "maximum UTF-8 cursor":
				cursor := strings.Repeat("x", 1025)
				if mode == "maximum cursor" {
					cursor = strings.Repeat("x", domain.MaxNameEntryCursorLength)
				}
				if mode == "maximum UTF-8 cursor" {
					cursor = strings.Repeat("界", 682) + "aa"
				}
				result.NextCursor = &cursor
			case "minimum time", "maximum time", "local year normalizes into range", "submicrosecond time":
				stamp := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				switch mode {
				case "maximum time":
					stamp = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
				case "local year normalizes into range":
					stamp = time.Date(10000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 3600))
				case "submicrosecond time":
					stamp = time.Date(2026, 11, 17, 2, 30, 0, 999, time.UTC)
				}
				for index := range result.Entries {
					entry := &result.Entries[index].Entry
					entry.CreatedAt, entry.ModifiedAt, entry.UpdatedAt = stamp, stamp, stamp
				}
			case "binary keys":
				result.Entries[0].NameKey = []byte{0}
				result.Entries[1].NameKey = bytes.Repeat([]byte{0xff}, 768)
			case "display and order":
				result.Entries[0].Entry.Name = "  Cafe\u0301.MD 🌱  "
				result.Entries[0].Entry.ID, result.Entries[1].Entry.ID = 29, 23
			}
			original := append([]domain.NameEntryCandidate(nil), result.Entries...)
			for index := range original {
				original[index].NameKey = bytes.Clone(original[index].NameKey)
				parentID := *original[index].Entry.ParentID
				original[index].Entry.ParentID = &parentID
			}
			data, err := NewNameEntryPageData(result)
			if err != nil || data == nil || len(data.Items) != len(result.Entries) {
				t.Fatal("合法名称分页边界被拒绝", err)
			}
			for index, item := range data.Items {
				entry := result.Entries[index].Entry
				if item.ID != strconv.FormatUint(entry.ID, 10) || item.ParentID != strconv.FormatUint(*entry.ParentID, 10) || item.Revision != strconv.FormatUint(entry.Revision, 10) || item.SizeBytes != entry.FileSize || item.Name != entry.Name || item.EntryType != entry.EntryType || item.Status != entry.Status {
					t.Fatal("名称分页标识、顺序或展示值转换错误")
				}
				for _, stamp := range []struct{ source, converted time.Time }{
					{entry.CreatedAt, item.CreatedAt}, {entry.ModifiedAt, item.ModifiedAt}, {entry.UpdatedAt, item.UpdatedAt},
				} {
					if stamp.converted.Location() != time.UTC || stamp.converted.Nanosecond()%1000 != 0 || !stamp.converted.Equal(stamp.source.UTC().Truncate(time.Microsecond)) {
						t.Fatal("名称分页 UTC 微秒时间转换错误")
					}
				}
			}
			if (data.NextCursor != nil) != (result.NextCursor != nil) || (data.NextCursor != nil && *data.NextCursor != *result.NextCursor) {
				t.Fatal("名称分页边界游标原文错误")
			}
			if _, err := json.Marshal(data); err != nil {
				t.Fatal("名称分页边界无法编码", err)
			}
			if !reflect.DeepEqual(result.Entries, original) {
				t.Fatal("名称分页转换修改来源")
			}
		})
	}
}

func TestNewNameEntryPageDataFailures(t *testing.T) {
	for _, mode := range []string{
		"nil result", "large page", "empty cursor", "large cursor", "large UTF-8 cursor", "zero owner", "mixed owners", "zero id", "nil parent",
		"zero parent", "zero revision", "empty name", "inactive", "bad type", "directory size", "unsafe size", "bad later item",
	} {
		t.Run(mode, func(t *testing.T) {
			result := nameEntryPageDataFixture()
			switch mode {
			case "nil result":
				result = nil
			case "large page":
				result.Entries = make([]domain.NameEntryCandidate, 101)
			case "empty cursor", "large cursor", "large UTF-8 cursor":
				cursor := ""
				if mode == "large cursor" {
					cursor = strings.Repeat("x", domain.MaxNameEntryCursorLength+1)
				}
				if mode == "large UTF-8 cursor" {
					cursor = strings.Repeat("界", 683)
				}
				result.NextCursor = &cursor
			case "zero owner":
				result.Entries[0].Entry.UserID = 0
			case "mixed owners":
				result.Entries[1].Entry.UserID++
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
			case "directory size":
				result.Entries[1].Entry.FileSize = 1
			case "unsafe size":
				result.Entries[0].Entry.FileSize = maxJSONSafeInteger + 1
			case "bad later item":
				result.Entries[1].Entry.ModifiedAt = time.Time{}
			}
			if data, err := NewNameEntryPageData(result); data != nil || err == nil || (mode != "unsafe size" && !errors.Is(err, domain.ErrInvalidFileEntryData)) {
				t.Fatal("名称分页异常被接受或返回部分数据", err)
			}
		})
	}
	// 1. 分别核对三项时间的 UTC 年份下界和上界。
	for _, field := range []string{"created", "modified", "updated"} {
		for _, stamp := range []time.Time{
			time.Time{}, time.Date(999, 12, 31, 23, 59, 59, 999999999, time.UTC), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
			time.Date(1000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 3600)), time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600)),
		} {
			result := nameEntryPageDataFixture()
			switch field {
			case "created":
				result.Entries[1].Entry.CreatedAt = stamp
			case "modified":
				result.Entries[1].Entry.ModifiedAt = stamp
			case "updated":
				result.Entries[1].Entry.UpdatedAt = stamp
			}
			if data, err := NewNameEntryPageData(result); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("名称分页异常 UTC 日期被接受", field, err)
			}
		}
	}
}

func TestNewNameEntryPageDataCopies(t *testing.T) {
	// 1. 两次转换分别持有条目、导航和游标值。
	result := nameEntryPageDataFixture()
	cursor := "fixture-next"
	result.NextCursor = &cursor
	expectedCursor := cursor
	originalParent := *result.Entries[0].Entry.ParentID
	originalKey := bytes.Clone(result.Entries[0].NameKey)
	first, err := NewNameEntryPageData(result)
	if err != nil {
		t.Fatal(err)
	}
	expected := append([]DirectoryEntrySummary(nil), first.Items...)
	first.Items[0].Name, first.Items[0].ParentID = "本地修改", "999"
	first.Items[0].ModifiedAt = time.Time{}
	*first.NextCursor = "local-next"
	first.Breadcrumbs = append(first.Breadcrumbs, DirectoryBreadcrumb{ID: "999", Name: "本地"})
	second, err := NewNameEntryPageData(result)
	if err != nil || second == nil || !reflect.DeepEqual(second.Items, expected) || *second.NextCursor != expectedCursor || len(second.Breadcrumbs) != 0 || *result.Entries[0].Entry.ParentID != originalParent || !bytes.Equal(result.Entries[0].NameKey, originalKey) || first.NextCursor == second.NextCursor || first.NextCursor == result.NextCursor {
		t.Fatal("名称分页 DTO 与来源或其他结果互相影响", err)
	}
	// 2. 来源及键在转换后变化，已生成的公开字段保留各自值。
	result.Entries[0].Entry.Name = "来源后续修改"
	*result.Entries[0].Entry.ParentID = 999
	result.Entries[0].Entry.ModifiedAt = time.Time{}
	result.Entries[0].NameKey[0] ^= 1
	*result.NextCursor = "source-next"
	if !reflect.DeepEqual(second.Items, expected) || *second.NextCursor != expectedCursor {
		t.Fatal("来源修改影响已有名称分页响应")
	}
}
