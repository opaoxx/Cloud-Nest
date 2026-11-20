package dto

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const purgePreviewExampleDigest = "eyJ2IjoxfQ.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func purgePreviewResponseFixture() domain.PurgePreview {
	return domain.PurgePreview{
		EntryID: 23, Revision: 4,
		Summary: domain.PurgePreviewSummary{FileCount: 2, DirectoryCount: 1, LogicalBytes: 6144},
		Credential: domain.PurgePreviewCredential{
			PreviewDigest: purgePreviewExampleDigest,
			ExpiresAt:     time.Date(2026, 11, 20, 12, 35, 0, 123456000, time.FixedZone("UTC+08", 8*3600)),
		},
	}
}

func TestNewPurgePreviewDataJSON(t *testing.T) {
	// 1. 核对示例的完整 JSON、数据类型和时区转换。
	result := purgePreviewResponseFixture()
	data, err := NewPurgePreviewData(&result)
	if err != nil || data == nil || data.ExpiresAt.Location() != time.UTC ||
		!data.ExpiresAt.Equal(result.Credential.ExpiresAt) {
		t.Fatal("UTC 截止时间或预检转换不符", data, err)
	}
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	response := PurgePreviewResponse{Data: *data, RequestID: requestID}
	const want = `{"data":{"entry_id":"23","revision":"4","file_count":2,"directory_count":1,"logical_bytes":"6144","preview_digest":"` +
		purgePreviewExampleDigest + `","expires_at":"2026-11-20T04:35:00.123456Z"},"request_id":"` + requestID + `"}`
	body, err := json.Marshal(response)
	if err != nil || string(body) != want {
		t.Fatalf("预检响应为 %s，期望 %s，错误 %v", body, want, err)
	}
}

func TestNewPurgePreviewDataBounds(t *testing.T) {
	// 1. 最大无符号字段和 10000 项范围均能完整交付。
	result := purgePreviewResponseFixture()
	result.EntryID = math.MaxUint64
	result.Revision = math.MaxUint64
	result.Summary = domain.PurgePreviewSummary{
		FileCount: domain.MaxFileEntrySubtreeNodes, LogicalBytes: math.MaxUint64,
	}
	data, err := NewPurgePreviewData(&result)
	if err != nil || data == nil || data.EntryID != "18446744073709551615" ||
		data.Revision != "18446744073709551615" || data.LogicalBytes != "18446744073709551615" ||
		data.FileCount != domain.MaxFileEntrySubtreeNodes || data.DirectoryCount != 0 {
		t.Fatal("最大范围或完整无符号值不符", data, err)
	}

	// 2. 空目录本身计为一项，逻辑用量以字符串 "0" 返回。
	result.Summary = domain.PurgePreviewSummary{DirectoryCount: 1}
	data, err = NewPurgePreviewData(&result)
	if err != nil || data == nil || data.LogicalBytes != "0" || data.FileCount != 0 || data.DirectoryCount != 1 {
		t.Fatal("空目录预检转换不符", data, err)
	}
}

func TestNewPurgePreviewDataRejectsInvalidResult(t *testing.T) {
	if data, err := NewPurgePreviewData(nil); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
		t.Fatal("空预检结果应被拒绝", data, err)
	}
	for _, scenario := range []struct {
		name string
		edit func(*domain.PurgePreview)
	}{
		{"zero entry ID", func(result *domain.PurgePreview) { result.EntryID = 0 }},
		{"zero revision", func(result *domain.PurgePreview) { result.Revision = 0 }},
		{"negative file count", func(result *domain.PurgePreview) { result.Summary.FileCount = -1 }},
		{"negative directory count", func(result *domain.PurgePreview) { result.Summary.DirectoryCount = -1 }},
		{"out of range count", func(result *domain.PurgePreview) { result.Summary.FileCount = math.MaxInt }},
		{"combined count too high", func(result *domain.PurgePreview) { result.Summary.DirectoryCount = domain.MaxFileEntrySubtreeNodes }},
		{"empty scope", func(result *domain.PurgePreview) { result.Summary = domain.PurgePreviewSummary{} }},
		{"bytes without files", func(result *domain.PurgePreview) { result.Summary.FileCount = 0 }},
		{"empty credential", func(result *domain.PurgePreview) { result.Credential.PreviewDigest = "" }},
		{"wrong separator", func(result *domain.PurgePreview) {
			result.Credential.PreviewDigest = strings.Replace(purgePreviewExampleDigest, ".", ":", 1)
		}},
		{"padded credential", func(result *domain.PurgePreview) { result.Credential.PreviewDigest += "=" }},
		{"truncated MAC", func(result *domain.PurgePreview) {
			result.Credential.PreviewDigest = purgePreviewExampleDigest[:len(purgePreviewExampleDigest)-1]
		}},
		{"credential too long", func(result *domain.PurgePreview) {
			result.Credential.PreviewDigest = strings.Repeat("A", domain.MaxPurgePreviewDigestLength-len(purgePreviewExampleDigest)+1) +
				purgePreviewExampleDigest
		}},
		{"zero expiry", func(result *domain.PurgePreview) { result.Credential.ExpiresAt = time.Time{} }},
		{"invalid UTC year", func(result *domain.PurgePreview) {
			result.Credential.ExpiresAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.FixedZone("UTC+08", 8*3600))
		}},
		{"submicrosecond expiry", func(result *domain.PurgePreview) {
			result.Credential.ExpiresAt = result.Credential.ExpiresAt.Add(time.Nanosecond)
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result := purgePreviewResponseFixture()
			scenario.edit(&result)
			if data, err := NewPurgePreviewData(&result); data != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常预检结果应被拒绝", data, err)
			}
		})
	}
}
