package dto

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func renamedEntryDataFixture() *domain.EntryDetail {
	result := entryDetailDataFixture()
	target := &result.Path.Entries[len(result.Path.Entries)-1]
	target.Name, target.Revision = "学习笔记.PDF", 4
	target.ModifiedAt, target.UpdatedAt = target.ModifiedAt.Add(24*time.Hour), target.UpdatedAt.Add(24*time.Hour)
	extension := "pdf"
	result.Display.Extension = &extension
	return result
}

func TestNewRenameEntryDataJSON(t *testing.T) {
	for _, mode := range []string{"file", "directory", "empty optional text", "null optional fields", "same name revision one"} {
		t.Run(mode, func(t *testing.T) {
			result := renamedEntryDataFixture()
			target := &result.Path.Entries[len(result.Path.Entries)-1]
			switch mode {
			case "directory":
				target.EntryType, target.FileSize = "DIR", 0
				result.Display.Category, result.Display.Extension = nil, nil
				color := "green"
				result.Display.ColorKey = &color
			case "empty optional text":
				empty := ""
				result.Display.Extension, result.Display.MIMEType, result.Display.ColorKey, result.Display.SourceDeviceName = &empty, &empty, &empty, &empty
			case "null optional fields":
				result.Display.Extension, result.Display.MIMEType, result.Display.ColorKey = nil, nil, nil
				result.Display.SourceDeviceID, result.Display.SourceDeviceName, result.Display.ContentModifiedAt = nil, nil, nil
				result.Preference.StarredAt, result.Preference.LastOpenedAt = nil, nil
			case "same name revision one":
				target.Revision = 1
			}
			// 1. 核对稳定 entry 快照与既有详情字段的一致映射。
			data, err := NewRenameEntryData(result)
			expected, expectedError := NewEntryDetailData(result)
			if err != nil || expectedError != nil || data == nil || !reflect.DeepEqual(data.Entry, *expected) {
				t.Fatal("重命名快照转换错误", err, expectedError)
			}
			response := RenameEntryResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}
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
				t.Fatal("稳定数据字段错误")
			}
			var entry map[string]any
			if json.Unmarshal(payload["entry"], &entry) != nil || len(entry) != 18 || entry["id"] != "23" || entry["parent_id"] != "12" || entry["name"] != target.Name || entry["revision"] != strconv.FormatUint(target.Revision, 10) {
				t.Fatal("公开快照字段或类型错误")
			}
			if mode == "file" && (entry["extension"] != "pdf" || entry["kind"] != "document" || entry["mime_type"] != "text/markdown" || entry["modified_at"] != "2026-11-16T02:30:00.123456Z" || entry["content_modified_at"] != "2026-11-15T01:30:00.123456Z") {
				t.Fatal("文件派生字段或时间错误")
			}
			if mode == "directory" && (entry["kind"] != "folder" || entry["size_bytes"] != float64(0) || entry["extension"] != nil || entry["color_key"] != "green") {
				t.Fatal("目录快照错误")
			}
			if mode == "null optional fields" {
				for _, field := range []string{"extension", "mime_type", "color_key", "content_modified_at", "source_device", "starred_at", "last_opened_at"} {
					if value, exists := entry[field]; !exists || value != nil {
						t.Fatal("可空字段错误", field)
					}
				}
				if entry["starred"] != false {
					t.Fatal("收藏标记错误")
				}
			}
			// 2. 同一快照可以关联各次请求自己的 ID。
			next := RenameEntryResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e149"}
			if next.RequestID == response.RequestID || !reflect.DeepEqual(next.Data, response.Data) {
				t.Fatal("稳定快照与请求 ID 组合错误")
			}
		})
	}
}

func TestNewRenameEntryDataBoundsAndCopies(t *testing.T) {
	result := renamedEntryDataFixture()
	target := &result.Path.Entries[len(result.Path.Entries)-1]
	maximum := ^uint64(0)
	target.ID, target.Revision, target.FileSize = maximum, maximum, maxJSONSafeInteger
	result.Display.ID, result.Preference.EntryID = maximum, maximum
	result.Display.SourceDeviceID = &maximum
	color := "green"
	result.Display.ColorKey = &color
	target.Name = "Cafe\u0301.PDF"
	data, err := NewRenameEntryData(result)
	if err != nil || data.Entry.ID != "18446744073709551615" || data.Entry.Revision != "18446744073709551615" || data.Entry.SourceDevice.ID == nil || *data.Entry.SourceDevice.ID != "18446744073709551615" || data.Entry.SizeBytes != maxJSONSafeInteger || data.Entry.Name != target.Name {
		t.Fatal("完整整数或展示名称错误", err)
	}
	before, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	// 修改所有输入指针后，已转换快照仍由调用方独立持有。
	*target.ParentID = 999
	target.Name = "later name"
	for _, text := range []*string{result.Display.Category, result.Display.Extension, result.Display.MIMEType, result.Display.ColorKey, result.Display.SourceDeviceName} {
		*text = "later text"
	}
	*result.Display.SourceDeviceID = 1
	*result.Display.ContentModifiedAt = time.Now()
	*result.Preference.StarredAt = time.Now()
	*result.Preference.LastOpenedAt = time.Now()
	after, err := json.Marshal(data)
	if err != nil || string(before) != string(after) {
		t.Fatal("快照受到输入修改影响", err)
	}
	for _, stamp := range []time.Time{
		time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
		time.Date(10000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 3600)),
		time.Date(999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600)),
	} {
		result := renamedEntryDataFixture()
		target := &result.Path.Entries[len(result.Path.Entries)-1]
		target.CreatedAt, target.ModifiedAt, target.UpdatedAt = stamp, stamp, stamp
		data, err := NewRenameEntryData(result)
		if err != nil || data == nil {
			t.Fatal("合法年份边界拒绝", err)
		}
		for _, actual := range []time.Time{data.Entry.CreatedAt, data.Entry.ModifiedAt, data.Entry.UpdatedAt} {
			if actual.Location() != time.UTC || !actual.Equal(stamp.UTC().Truncate(time.Microsecond)) {
				t.Fatal("UTC 微秒转换错误")
			}
		}
	}
}

func TestNewRenameEntryDataFailures(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		change func(*domain.EntryDetail) *domain.EntryDetail
	}{
		{"nil", func(*domain.EntryDetail) *domain.EntryDetail { return nil }},
		{"empty path", func(result *domain.EntryDetail) *domain.EntryDetail { result.Path.Entries = nil; return result }},
		{"root", func(result *domain.EntryDetail) *domain.EntryDetail {
			result.Path.Entries = result.Path.Entries[:1]
			return result
		}},
		{"empty name", func(result *domain.EntryDetail) *domain.EntryDetail { result.Path.Entries[2].Name = ""; return result }},
		{"invalid utf8", func(result *domain.EntryDetail) *domain.EntryDetail {
			result.Path.Entries[2].Name = "private-name\xff"
			return result
		}},
		{"zero version", func(result *domain.EntryDetail) *domain.EntryDetail {
			result.Path.Entries[2].Revision = 0
			return result
		}},
		{"hidden target", func(result *domain.EntryDetail) *domain.EntryDetail {
			result.Path.Entries[2].Status = "TRASHED"
			return result
		}},
		{"hidden ancestor", func(result *domain.EntryDetail) *domain.EntryDetail {
			result.Path.Entries[1].Status = "PURGED"
			return result
		}},
		{"foreign target", func(result *domain.EntryDetail) *domain.EntryDetail {
			result.Path.Entries[2].UserID++
			return result
		}},
		{"wrong parent", func(result *domain.EntryDetail) *domain.EntryDetail {
			*result.Path.Entries[2].ParentID = 999
			return result
		}},
		{"display scope", func(result *domain.EntryDetail) *domain.EntryDetail {
			result.Display.ID++
			return result
		}},
		{"preference scope", func(result *domain.EntryDetail) *domain.EntryDetail {
			result.Preference.UserID++
			return result
		}},
		{"unknown category", func(result *domain.EntryDetail) *domain.EntryDetail {
			category := "UNKNOWN"
			result.Display.Category = &category
			return result
		}},
		{"zero time", func(result *domain.EntryDetail) *domain.EntryDetail {
			result.Path.Entries[2].ModifiedAt = time.Time{}
			return result
		}},
		{"out of range time", func(result *domain.EntryDetail) *domain.EntryDetail {
			result.Path.Entries[2].UpdatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			return result
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			data, err := NewRenameEntryData(testCase.change(renamedEntryDataFixture()))
			if data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常详情未拒绝", err)
			}
		})
	}
	result := renamedEntryDataFixture()
	result.Path.Entries[2].FileSize = maxJSONSafeInteger + 1
	if data, err := NewRenameEntryData(result); data != nil || err == nil {
		t.Fatal("超出 JSON 安全范围的大小未拒绝", err)
	}
}
