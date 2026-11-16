package dto

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestNewRecordAccessDataJSON(t *testing.T) {
	for _, mode := range []string{"access only", "with star", "maximum identifiers"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备已提交访问结果，保存来源原值。
			stamp := time.Date(2026, 11, 16, 10, 30, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
			result := domain.EntryPreference{UserID: 7, EntryID: 13, LastOpenedAt: &stamp}
			if mode == "with star" {
				result.StarredAt = &stamp
			}
			if mode == "maximum identifiers" {
				result.UserID, result.EntryID = ^uint64(0), ^uint64(0)
			}
			before, originalStamp := result, stamp
			data, err := NewRecordAccessData(&result)
			if err != nil || data == nil || !reflect.DeepEqual(result, before) || stamp != originalStamp {
				t.Fatal("访问结果转换或输入原值错误", err)
			}

			// 2. 核对精确公开字段、当前请求 ID 和 UTC 微秒值。
			encoded, err := json.Marshal(RecordAccessResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"})
			expected := `{"data":{"last_opened_at":"2026-11-16T02:30:00.123456Z"},"request_id":"28ee758a-0727-42de-ae7d-3bb33155e148"}`
			if err != nil || string(encoded) != expected {
				t.Fatal("访问响应字段、时间或请求 ID 错误", err)
			}
		})
	}
}

func TestNewRecordAccessDataTimeBoundaries(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		stamp time.Time
	}{
		{"minimum", time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"maximum", time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)},
		{"local year normalizes into range", time.Date(10000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 3600))},
		{"nanoseconds truncate", time.Date(2026, 11, 16, 2, 30, 0, 999, time.UTC)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := domain.EntryPreference{UserID: 7, EntryID: 13, LastOpenedAt: &testCase.stamp}
			data, err := NewRecordAccessData(&result)
			if err != nil || data == nil || data.LastOpenedAt.Location() != time.UTC || !data.LastOpenedAt.Equal(testCase.stamp.UTC().Truncate(time.Microsecond)) || data.LastOpenedAt.Nanosecond()%1000 != 0 {
				t.Fatal("合法访问时间转换错误", err)
			}
			if _, err := json.Marshal(data); err != nil {
				t.Fatal("访问时间编码失败", err)
			}
		})
	}
}

func TestNewRecordAccessDataFailures(t *testing.T) {
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	for _, testCase := range []struct {
		name   string
		result *domain.EntryPreference
	}{
		{"nil", nil},
		{"zero user", &domain.EntryPreference{EntryID: 13, LastOpenedAt: &stamp}},
		{"zero entry", &domain.EntryPreference{UserID: 7, LastOpenedAt: &stamp}},
		{"missing access", &domain.EntryPreference{UserID: 7, EntryID: 13}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			data, err := NewRecordAccessData(testCase.result)
			if data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常结果或缺失访问未拒绝", err)
			}
		})
	}
	for _, testCase := range []struct {
		name  string
		stamp time.Time
	}{
		{name: "zero time"},
		{"year too small", time.Date(999, 12, 31, 23, 59, 59, 999999999, time.UTC)},
		{"year too large", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"UTC year below range", time.Date(1000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 3600))},
		{"UTC year above range", time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600))},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := domain.EntryPreference{UserID: 7, EntryID: 13, LastOpenedAt: &testCase.stamp}
			before := result
			data, err := NewRecordAccessData(&result)
			if data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) || !reflect.DeepEqual(result, before) {
				t.Fatal("非法访问时间或输入保护错误", err)
			}
		})
	}
}

func TestNewRecordAccessDataCopies(t *testing.T) {
	// 1. 两次转换持有独立的时间值，来源时区与精度有原值对照。
	stamp := time.Date(2026, 11, 16, 10, 30, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
	original := stamp
	result := domain.EntryPreference{UserID: 7, EntryID: 13, LastOpenedAt: &stamp, StarredAt: &stamp}
	first, err := NewRecordAccessData(&result)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRecordAccessData(&result)
	if err != nil || first == second || stamp != original {
		t.Fatal("独立结果或输入原值错误", err)
	}
	// 2. 修改响应和来源，各次转换分别保有自己的时间。
	first.LastOpenedAt = first.LastOpenedAt.Add(time.Hour)
	if stamp != original || !second.LastOpenedAt.Equal(original.UTC().Truncate(time.Microsecond)) {
		t.Fatal("修改响应影响来源或另一次结果")
	}
	stamp = stamp.Add(2 * time.Hour)
	if !second.LastOpenedAt.Equal(original.UTC().Truncate(time.Microsecond)) {
		t.Fatal("来源修改影响已有响应")
	}
}
