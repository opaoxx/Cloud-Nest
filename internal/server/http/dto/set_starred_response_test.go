package dto

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestNewSetStarredDataJSON(t *testing.T) {
	for _, mode := range []string{"starred", "unstarred", "recent access only", "maximum identifiers"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备已提交偏好，保留内部归属和访问时间。
			stamp := time.Date(2026, 11, 16, 10, 30, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
			result := domain.EntryPreference{UserID: 7, EntryID: 13, StarredAt: &stamp, LastOpenedAt: &stamp}
			expected := `{"data":{"starred":true,"starred_at":"2026-11-16T02:30:00.123456Z"},"request_id":"28ee758a-0727-42de-ae7d-3bb33155e148"}`
			switch mode {
			case "unstarred":
				result.StarredAt, result.LastOpenedAt = nil, nil
			case "recent access only":
				result.StarredAt = nil
			case "maximum identifiers":
				result.UserID, result.EntryID = ^uint64(0), ^uint64(0)
			}
			if result.StarredAt == nil {
				expected = `{"data":{"starred":false,"starred_at":null},"request_id":"28ee758a-0727-42de-ae7d-3bb33155e148"}`
			}

			// 2. 转换并编码完整响应，核对精确字段和 UTC 微秒值。
			before := result
			originalStamp := stamp
			data, err := NewSetStarredData(&result)
			if err != nil || data == nil || !reflect.DeepEqual(result, before) || stamp != originalStamp {
				t.Fatal("收藏偏好转换或输入原值错误", err)
			}
			encoded, err := json.Marshal(SetStarredResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"})
			if err != nil || string(encoded) != expected {
				t.Fatal("收藏响应字段、布尔值、时间或 null 错误", err)
			}
		})
	}
}

func TestNewSetStarredDataTimeBoundaries(t *testing.T) {
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
			result := domain.EntryPreference{UserID: 7, EntryID: 13, StarredAt: &testCase.stamp}
			data, err := NewSetStarredData(&result)
			if err != nil || data == nil || !data.Starred || data.StarredAt == nil || data.StarredAt.Location() != time.UTC ||
				!data.StarredAt.Equal(testCase.stamp.UTC().Truncate(time.Microsecond)) || data.StarredAt.Nanosecond()%1000 != 0 {
				t.Fatal("合法时间转换错误", err)
			}
			if _, err := json.Marshal(data); err != nil {
				t.Fatal("规范化时间编码失败", err)
			}
		})
	}
}

func TestNewSetStarredDataFailures(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		result *domain.EntryPreference
	}{
		{"nil", nil},
		{"zero user", &domain.EntryPreference{EntryID: 13}},
		{"zero entry", &domain.EntryPreference{UserID: 7}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			data, err := NewSetStarredData(testCase.result)
			if data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("无效结果或标识未拒绝", err)
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
			result := domain.EntryPreference{UserID: 7, EntryID: 13, StarredAt: &testCase.stamp}
			before := result
			data, err := NewSetStarredData(&result)
			if data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) || !reflect.DeepEqual(result, before) {
				t.Fatal("非法收藏时间或原值保护错误", err)
			}
		})
	}
}

func TestNewSetStarredDataCopies(t *testing.T) {
	// 1. 两次转换取得独立时间，转换过程保留来源时区和精度。
	stamp := time.Date(2026, 11, 16, 10, 30, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
	result := domain.EntryPreference{UserID: 7, EntryID: 13, StarredAt: &stamp, LastOpenedAt: &stamp}
	original := stamp
	first, err := NewSetStarredData(&result)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSetStarredData(&result)
	if err != nil || first.StarredAt == result.StarredAt || second.StarredAt == result.StarredAt || first.StarredAt == second.StarredAt || stamp != original {
		t.Fatal("时间副本或来源原值错误", err)
	}

	// 2. 修改响应及来源，两次转换结果分别保留自己的事实。
	first.Starred, *first.StarredAt = false, stamp.Add(time.Hour)
	if stamp != original || !second.Starred || !second.StarredAt.Equal(original.UTC().Truncate(time.Microsecond)) {
		t.Fatal("修改响应影响来源或另一次转换")
	}
	stamp = stamp.Add(2 * time.Hour)
	if !second.StarredAt.Equal(original.UTC().Truncate(time.Microsecond)) {
		t.Fatal("修改来源影响既有响应")
	}
}
