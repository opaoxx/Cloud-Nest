package dto

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func restoredEntryResponseFixture() *domain.FileEntry {
	parentID := uint64(11)
	return &domain.FileEntry{
		ID: 23, UserID: 7, ParentID: &parentID, Name: "学习笔记 (1).md",
		EntryType: "FILE", FileSize: 2048, Revision: 5, Status: "ACTIVE",
	}
}

func TestNewRestoreEntryDataJSON(t *testing.T) {
	for _, scenario := range []struct {
		name, expectedEntry string
		edit                func(*domain.FileEntry)
	}{
		{
			name:          "file from contract",
			expectedEntry: `{"id":"23","parent_id":"11","name":"学习笔记 (1).md","entry_type":"FILE","status":"ACTIVE","revision":"5"}`,
		},
		{
			name: "directory preserves display spelling",
			edit: func(result *domain.FileEntry) {
				result.EntryType, result.FileSize, result.Name = "DIR", 0, "Cafe\u0301.V2 (1) 🌱"
			},
			expectedEntry: `{"id":"23","parent_id":"11","name":"Café.V2 (1) 🌱","entry_type":"DIR","status":"ACTIVE","revision":"5"}`,
		},
		{
			name: "full unsigned identifiers and revision",
			edit: func(result *domain.FileEntry) {
				result.ID, *result.ParentID, result.Revision = math.MaxUint64, math.MaxUint64-1, math.MaxUint64
			},
			expectedEntry: `{"id":"18446744073709551615","parent_id":"18446744073709551614","name":"学习笔记 (1).md","entry_type":"FILE","status":"ACTIVE","revision":"18446744073709551615"}`,
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result := restoredEntryResponseFixture()
			if scenario.edit != nil {
				scenario.edit(result)
			}
			data, err := NewRestoreEntryData(result)
			if err != nil || data == nil {
				t.Fatal("无法转换恢复结果", data, err)
			}
			wantData := `{"entry":` + scenario.expectedEntry + `}`
			body, err := json.Marshal(data)
			if err != nil || string(body) != wantData {
				t.Fatalf("稳定 JSON 为 %s，期望 %s，错误 %v", body, wantData, err)
			}

			// 1. 改动内部对象后，复用的快照保留最初的结果。
			*result.ParentID, result.Name, result.Status, result.Revision = 99, "later change", "TRASHED", 1
			// 2. 每次交付加上各自的请求 ID，data 字段相同。
			for _, requestID := range []string{"28ee758a-0727-42de-ae7d-3bb33155e148", "28ee758a-0727-42de-ae7d-3bb33155e149"} {
				encoded, err := json.Marshal(RestoreEntryResponse{Data: *data, RequestID: requestID})
				want := `{"data":` + wantData + `,"request_id":"` + requestID + `"}`
				if err != nil || string(encoded) != want {
					t.Fatalf("恢复响应为 %s，期望 %s，错误 %v", encoded, want, err)
				}
			}
		})
	}
}

func TestNewRestoreEntryDataRejectsInvalidResult(t *testing.T) {
	if data, err := NewRestoreEntryData(nil); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
		t.Fatal("空结果未拒绝", data, err)
	}
	for _, scenario := range []struct {
		name string
		edit func(*domain.FileEntry)
	}{
		{"zero entry ID", func(result *domain.FileEntry) { result.ID = 0 }},
		{"zero owner", func(result *domain.FileEntry) { result.UserID = 0 }},
		{"missing parent", func(result *domain.FileEntry) { result.ParentID = nil }},
		{"zero parent", func(result *domain.FileEntry) { *result.ParentID = 0 }},
		{"self parent", func(result *domain.FileEntry) { *result.ParentID = result.ID }},
		{"empty name", func(result *domain.FileEntry) { result.Name = "" }},
		{"invalid UTF-8", func(result *domain.FileEntry) { result.Name = "\xff" }},
		{"unknown entry type", func(result *domain.FileEntry) { result.EntryType = "OTHER" }},
		{"directory with file size", func(result *domain.FileEntry) { result.EntryType = "DIR" }},
		{"trashed result", func(result *domain.FileEntry) { result.Status = "TRASHED" }},
		{"purging result", func(result *domain.FileEntry) { result.Status = "PURGING" }},
		{"purged result", func(result *domain.FileEntry) { result.Status = "PURGED" }},
		{"zero revision", func(result *domain.FileEntry) { result.Revision = 0 }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result := restoredEntryResponseFixture()
			scenario.edit(result)
			if data, err := NewRestoreEntryData(result); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常数据被转成成功快照", data, err)
			}
		})
	}
}
