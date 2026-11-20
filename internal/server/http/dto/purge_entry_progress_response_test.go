package dto

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestNewPurgeEntryProgressDataJSON(t *testing.T) {
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	for _, scenario := range []struct {
		name   string
		result domain.PurgeProgress
		data   string
	}{
		{
			name:   "only target remains",
			result: domain.PurgeProgress{EntryID: 23, Status: "PURGING", RemainingCount: 1},
			data:   `{"entry_id":"23","status":"PURGING","remaining_count":1}`,
		},
		{
			name:   "maximum ID and count",
			result: domain.PurgeProgress{EntryID: math.MaxUint64, Status: "PURGING", RemainingCount: 10000},
			data:   `{"entry_id":"18446744073709551615","status":"PURGING","remaining_count":10000}`,
		},
		{
			name:   "completed with explicit zero",
			result: domain.PurgeProgress{EntryID: 23, Status: "PURGED", RemainingCount: 0},
			data:   `{"entry_id":"23","status":"PURGED","remaining_count":0}`,
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 转换领域结果，核对三个对外字段和外层请求 ID。
			data, err := NewPurgeEntryProgressData(&scenario.result)
			if err != nil || data == nil {
				t.Fatal("有效进度转换失败", err)
			}
			body, err := json.Marshal(PurgeEntryProgressResponse{Data: *data, RequestID: requestID})
			want := `{"data":` + scenario.data + `,"request_id":"` + requestID + `"}`
			if err != nil || string(body) != want {
				t.Fatalf("进度 JSON 为 %s，预期 %s，错误 %v", body, want, err)
			}
		})
	}
}

func TestNewPurgeEntryProgressDataRejectsInvalidResult(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		result *domain.PurgeProgress
	}{
		{"missing result", nil},
		{"zero ID", &domain.PurgeProgress{Status: "PURGED"}},
		{"purging with zero", &domain.PurgeProgress{EntryID: 23, Status: "PURGING"}},
		{"purging with negative count", &domain.PurgeProgress{EntryID: 23, Status: "PURGING", RemainingCount: -1}},
		{"purging over limit", &domain.PurgeProgress{EntryID: 23, Status: "PURGING", RemainingCount: 10001}},
		{"completed with remaining entry", &domain.PurgeProgress{EntryID: 23, Status: "PURGED", RemainingCount: 1}},
		{"completed with negative count", &domain.PurgeProgress{EntryID: 23, Status: "PURGED", RemainingCount: -1}},
		{"not yet accepted", &domain.PurgeProgress{EntryID: 23, Status: "TRASHED", RemainingCount: 1}},
		{"empty status", &domain.PurgeProgress{EntryID: 23}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 异常结果应返回统一内部错误类别。
			data, err := NewPurgeEntryProgressData(scenario.result)
			if data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("非法进度未被拒绝", data, err)
			}
		})
	}
}
