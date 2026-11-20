package dto

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func trashEntryDataFixture() *domain.TrashedFileEntry {
	deletedAt := time.Date(2026, 11, 20, 12, 30, 0, 123456000, time.FixedZone("UTC+08", 8*3600))
	return &domain.TrashedFileEntry{
		FileEntry: domain.FileEntry{
			ID: 23, UserID: 7, ParentID: new(uint64(12)), Name: "学习笔记.md",
			EntryType: "FILE", FileSize: 2048, Revision: 4, Status: "TRASHED",
		},
		DeletedAt: deletedAt,
	}
}

func TestNewTrashEntryDataJSON(t *testing.T) {
	for _, scenario := range []struct {
		name, expectedEntry string
		edit                func(*domain.TrashedFileEntry)
	}{
		{
			name:          "file",
			expectedEntry: `{"id":"23","parent_id":"12","name":"学习笔记.md","entry_type":"FILE","status":"TRASHED","revision":"4","deleted_at":"2026-11-20T04:30:00.123456Z"}`,
		},
		{
			name: "directory with legacy display name",
			edit: func(result *domain.TrashedFileEntry) {
				result.EntryType, result.FileSize, result.Name = "DIR", 0, "  Cafe\u0301 🌱  "
			},
			expectedEntry: `{"id":"23","parent_id":"12","name":"  Café 🌱  ","entry_type":"DIR","status":"TRASHED","revision":"4","deleted_at":"2026-11-20T04:30:00.123456Z"}`,
		},
		{
			name: "full unsigned identifiers",
			edit: func(result *domain.TrashedFileEntry) {
				result.ID, result.Revision, *result.ParentID = math.MaxUint64, math.MaxUint64, math.MaxUint64-1
			},
			expectedEntry: `{"id":"18446744073709551615","parent_id":"18446744073709551614","name":"学习笔记.md","entry_type":"FILE","status":"TRASHED","revision":"18446744073709551615","deleted_at":"2026-11-20T04:30:00.123456Z"}`,
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result := trashEntryDataFixture()
			if scenario.edit != nil {
				scenario.edit(result)
			}
			data, err := NewTrashEntryData(result)
			if err != nil || data == nil || data.Entry.DeletedAt.Location() != time.UTC {
				t.Fatal("回收快照转换失败或时间不为 UTC", data, err)
			}

			// 1. 核对稳定快照只交付七个公开字段，响应另加本次请求 ID。
			wantData := `{"entry":` + scenario.expectedEntry + `}`
			body, err := json.Marshal(data)
			if err != nil || string(body) != wantData {
				t.Fatal("稳定回收 JSON 字段或编码不符", string(body), err)
			}
			const firstID = "28ee758a-0727-42de-ae7d-3bb33155e148"
			const nextID = "28ee758a-0727-42de-ae7d-3bb33155e149"
			for _, id := range []string{firstID, nextID} {
				response, err := json.Marshal(TrashEntryResponse{Data: *data, RequestID: id})
				want := `{"data":` + wantData + `,"request_id":"` + id + `"}`
				if err != nil || string(response) != want {
					t.Fatal("回收响应未与当前请求 ID 关联", string(response), err)
				}
			}

			// 2. 转换后的父目录和名称独立于内部对象。
			first := data.Entry
			*result.ParentID, result.Name, result.DeletedAt = 99, "later change", time.Time{}
			if data.Entry != first {
				t.Fatal("内部对象变化影响了公开快照")
			}
		})
	}
}

func TestNewTrashEntryDataTimeBoundaries(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		source, want time.Time
	}{
		{"lower database year", time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"upper database year", time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)},
		{"truncate finer precision", time.Date(2026, 11, 20, 4, 30, 0, 123456999, time.UTC), time.Date(2026, 11, 20, 4, 30, 0, 123456000, time.UTC)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result := trashEntryDataFixture()
			result.DeletedAt = scenario.source
			original := *result
			data, err := NewTrashEntryData(result)
			if err != nil || data == nil || data.Entry.DeletedAt != scenario.want || !reflect.DeepEqual(result, &original) {
				t.Fatal("回收时间边界处理不符", data, err)
			}
		})
	}
}

func TestNewTrashEntryDataRejectsInvalidResult(t *testing.T) {
	if data, err := NewTrashEntryData(nil); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
		t.Fatal("空结果未拒绝", data, err)
	}
	for _, scenario := range []struct {
		name string
		edit func(*domain.TrashedFileEntry)
	}{
		{"zero ID", func(result *domain.TrashedFileEntry) { result.ID = 0 }},
		{"zero owner", func(result *domain.TrashedFileEntry) { result.UserID = 0 }},
		{"missing parent", func(result *domain.TrashedFileEntry) { result.ParentID = nil }},
		{"zero parent", func(result *domain.TrashedFileEntry) { *result.ParentID = 0 }},
		{"self parent", func(result *domain.TrashedFileEntry) { *result.ParentID = result.ID }},
		{"empty name", func(result *domain.TrashedFileEntry) { result.Name = "" }},
		{"invalid UTF-8", func(result *domain.TrashedFileEntry) { result.Name = "\xff" }},
		{"unknown entry type", func(result *domain.TrashedFileEntry) { result.EntryType = "OTHER" }},
		{"directory with file size", func(result *domain.TrashedFileEntry) { result.EntryType = "DIR" }},
		{"active status", func(result *domain.TrashedFileEntry) { result.Status = "ACTIVE" }},
		{"purging status", func(result *domain.TrashedFileEntry) { result.Status = "PURGING" }},
		{"zero revision", func(result *domain.TrashedFileEntry) { result.Revision = 0 }},
		{"zero deletion time", func(result *domain.TrashedFileEntry) { result.DeletedAt = time.Time{} }},
		{"UTC below database range", func(result *domain.TrashedFileEntry) {
			result.DeletedAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.FixedZone("UTC+01", 3600))
		}},
		{"UTC above database range", func(result *domain.TrashedFileEntry) {
			result.DeletedAt = time.Date(9999, 12, 31, 23, 0, 0, 0, time.FixedZone("UTC-01", -3600))
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result := trashEntryDataFixture()
			scenario.edit(result)
			// 1. 拒绝异常内部记录，保持统一的数据错误。
			if data, err := NewTrashEntryData(result); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常记录被转换为公开快照", data, err)
			}
		})
	}
}
