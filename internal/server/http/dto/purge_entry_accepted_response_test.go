package dto

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func purgeEntryAcceptedFixture() *domain.FileEntry {
	parentID := uint64(11)
	return &domain.FileEntry{
		ID: 23, UserID: 7, ParentID: &parentID, Name: "回收文件.md",
		EntryType: "FILE", FileSize: 2048, Status: "PURGING", Revision: 5,
	}
}

func TestNewPurgeEntryAcceptedDataJSON(t *testing.T) {
	for _, scenario := range []struct {
		name, idText string
		id           uint64
		directory    bool
	}{
		{"file from contract", "23", 23, false},
		{"directory", "1", 1, true},
		{"above JavaScript safe integer", "9007199254740993", 9007199254740993, false},
		{"maximum ID", "18446744073709551615", math.MaxUint64, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 按 OpenAPI 校验完整稳定字段和进度地址。
			entry := purgeEntryAcceptedFixture()
			entry.ID = scenario.id
			if scenario.directory {
				entry.EntryType, entry.FileSize, entry.Name = "DIR", 0, "回收目录"
			}
			data, err := NewPurgeEntryAcceptedData(entry)
			if err != nil || data == nil {
				t.Fatal("合法受理结果未转换", err)
			}
			want := `{"entry_id":"` + scenario.idText + `","status":"PURGING","progress_url":"/api/v1/trash/` + scenario.idText + `/purge"}`
			encoded, err := json.Marshal(data)
			if err != nil || string(encoded) != want {
				t.Fatalf("受理快照为 %s，期望 %s，错误 %v", encoded, want, err)
			}

			// 2. 后台状态推进后，稳定快照仍能用不同的请求 ID 交付。
			entry.ID, entry.Status = 99, "PURGED"
			for _, requestID := range []string{"28ee758a-0727-42de-ae7d-3bb33155e148", "28ee758a-0727-42de-ae7d-3bb33155e149"} {
				body, err := json.Marshal(PurgeEntryAcceptedResponse{Data: *data, RequestID: requestID})
				wantResponse := `{"data":` + want + `,"request_id":"` + requestID + `"}`
				if err != nil || string(body) != wantResponse {
					t.Fatalf("受理响应为 %s，期望 %s，错误 %v", body, wantResponse, err)
				}
			}
		})
	}
}

func TestNewPurgeEntryAcceptedDataRejectsInvalidResult(t *testing.T) {
	if data, err := NewPurgeEntryAcceptedData(nil); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
		t.Fatal("空受理结果应被拒绝", data, err)
	}
	for _, scenario := range []struct {
		name string
		edit func(*domain.FileEntry)
	}{
		{"zero ID", func(entry *domain.FileEntry) { entry.ID = 0 }},
		{"zero owner", func(entry *domain.FileEntry) { entry.UserID = 0 }},
		{"zero revision", func(entry *domain.FileEntry) { entry.Revision = 0 }},
		{"root", func(entry *domain.FileEntry) { entry.ParentID = nil }},
		{"zero parent", func(entry *domain.FileEntry) { *entry.ParentID = 0 }},
		{"self parent", func(entry *domain.FileEntry) { *entry.ParentID = entry.ID }},
		{"active", func(entry *domain.FileEntry) { entry.Status = "ACTIVE" }},
		{"trashed", func(entry *domain.FileEntry) { entry.Status = "TRASHED" }},
		{"purged", func(entry *domain.FileEntry) { entry.Status = "PURGED" }},
		{"unknown status", func(entry *domain.FileEntry) { entry.Status = "purging" }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			entry := purgeEntryAcceptedFixture()
			scenario.edit(entry)
			if data, err := NewPurgeEntryAcceptedData(entry); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常目录项应返回固定错误", data, err)
			}
		})
	}
}
