package dto

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func createDownloadResponseFixture() *domain.EntryDownloadAuthorization {
	return &domain.EntryDownloadAuthorization{
		Link: domain.SignedDownloadURL{
			URL:       "https://storage.example.invalid/bucket/note%2B.txt?X-Amz-Signature=fixture&versionId=old%2BA%2F%3D",
			ExpiresAt: time.Date(2026, 11, 20, 16, 15, 0, 123456789, time.FixedZone("UTC+8", 8*60*60)),
		},
		ObjectID: math.MaxUint64, Revision: 9007199254740993, SizeBytes: 0,
		VerifiedSHA256: [32]byte{0xab, 0x0f, 0xff},
		MIMEType:       "text/plain",
		Disposition:    `attachment; filename="download.txt"; filename*=UTF-8''%E5%AD%A6%E4%B9%A0.txt`,
	}
}

func TestNewCreateDownloadDataJSON(t *testing.T) {
	// 1. 构造授权，并通过 JSON 核对契约字段及实际编码。
	result := createDownloadResponseFixture()
	data, err := NewCreateDownloadData(result)
	if err != nil {
		t.Fatal(err)
	}
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	response, err := json.Marshal(CreateDownloadResponse{Data: *data, RequestID: requestID})
	if err != nil {
		t.Fatal("下载响应无法编码为 JSON", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(response, &decoded); err != nil || len(decoded) != 2 || string(decoded["request_id"]) != `"`+requestID+`"` {
		t.Fatal("下载响应外层字段不符", err)
	}
	var actual map[string]any
	if err := json.Unmarshal(decoded["data"], &actual); err != nil {
		t.Fatal("下载响应数据无法解析", err)
	}
	expected := map[string]any{
		"url": result.Link.URL, "expires_at": "2026-11-20T08:15:00.123456Z", "object_id": "18446744073709551615",
		"revision": "9007199254740993", "size_bytes": float64(0), "sha256": "ab0fff" + strings.Repeat("00", 29),
		"mime_type": "text/plain", "disposition": result.Disposition,
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatal("下载响应字段、类型或值偏离契约")
	}

	// 2. 修改原结果后，公开字段仍持有转换时的值。
	result.Link, result.VerifiedSHA256 = domain.SignedDownloadURL{}, [32]byte{}
	result.MIMEType, result.Disposition = "application/octet-stream", ""
	if data.URL != expected["url"] || data.SHA256 != expected["sha256"] || data.MIMEType != expected["mime_type"] ||
		data.Disposition != expected["disposition"] || data.ExpiresAt.Format(time.RFC3339Nano) != expected["expires_at"] {
		t.Fatal("源结果修改影响了已交付数据")
	}
}

func TestNewCreateDownloadDataBoundaries(t *testing.T) {
	for _, year := range []int{1000, 9999} {
		result := createDownloadResponseFixture()
		result.ObjectID, result.Revision, result.SizeBytes = 1, math.MaxUint64, 9007199254740991
		result.Link.URL = "http://[::1]:19000/cloudnest/object?X-Amz-Signature=fixture"
		result.Link.ExpiresAt = time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
		result.VerifiedSHA256, result.Disposition = [32]byte{}, `inline; filename="note.txt"; filename*=UTF-8''note.txt`
		data, err := NewCreateDownloadData(result)
		if err != nil || data.ObjectID != "1" || data.Revision != "18446744073709551615" || data.SizeBytes != result.SizeBytes ||
			data.SHA256 != strings.Repeat("00", 32) || data.URL != result.Link.URL || data.ExpiresAt != result.Link.ExpiresAt {
			t.Fatal("下载数据丢失边界事实", err)
		}
	}
}

func TestNewCreateDownloadDataRejectsInvalidResult(t *testing.T) {
	if data, err := NewCreateDownloadData(nil); data != nil || !errors.Is(err, domain.ErrInvalidDownloadAuthorizationData) {
		t.Fatal("空授权结果未被拒绝", err)
	}
	for _, scenario := range []struct {
		name string
		edit func(*domain.EntryDownloadAuthorization)
	}{
		{"zero object ID", func(result *domain.EntryDownloadAuthorization) { result.ObjectID = 0 }},
		{"zero revision", func(result *domain.EntryDownloadAuthorization) { result.Revision = 0 }},
		{"unsafe JSON size", func(result *domain.EntryDownloadAuthorization) { result.SizeBytes = 9007199254740992 }},
		{"missing deadline", func(result *domain.EntryDownloadAuthorization) { result.Link.ExpiresAt = time.Time{} }},
		{"deadline above UTC bound", func(result *domain.EntryDownloadAuthorization) {
			result.Link.ExpiresAt = time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -3600))
		}},
		{"deadline below UTC bound", func(result *domain.EntryDownloadAuthorization) {
			result.Link.ExpiresAt = time.Date(1000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 3600))
		}},
		{"relative URL", func(result *domain.EntryDownloadAuthorization) { result.Link.URL = "/private/file" }},
		{"non HTTP URL", func(result *domain.EntryDownloadAuthorization) { result.Link.URL = "file:///private/file" }},
		{"missing host", func(result *domain.EntryDownloadAuthorization) { result.Link.URL = "https:///private" }},
		{"URL credentials", func(result *domain.EntryDownloadAuthorization) {
			result.Link.URL = "https://private@example.invalid/file"
		}},
		{"URL fragment", func(result *domain.EntryDownloadAuthorization) { result.Link.URL += "#private" }},
		{"URL whitespace", func(result *domain.EntryDownloadAuthorization) { result.Link.URL += "\tprivate" }},
		{"unencoded URL path", func(result *domain.EntryDownloadAuthorization) { result.Link.URL = "https://example.invalid/文件" }},
		{"wrong MIME case", func(result *domain.EntryDownloadAuthorization) { result.MIMEType = "TEXT/PLAIN" }},
		{"MIME parameters", func(result *domain.EntryDownloadAuthorization) { result.MIMEType = "text/plain; charset=utf-8" }},
		{"MIME wildcard", func(result *domain.EntryDownloadAuthorization) { result.MIMEType = "image/*" }},
		{"overlong MIME", func(result *domain.EntryDownloadAuthorization) {
			result.MIMEType = "application/" + strings.Repeat("a", 116)
		}},
		{"wrong disposition kind", func(result *domain.EntryDownloadAuthorization) { result.Disposition = `form-data; filename="note.txt"` }},
		{"missing disposition filename", func(result *domain.EntryDownloadAuthorization) { result.Disposition = "inline; other=value" }},
		{"broken disposition syntax", func(result *domain.EntryDownloadAuthorization) { result.Disposition = `attachment; filename="note.txt` }},
		{"disposition line break", func(result *domain.EntryDownloadAuthorization) { result.Disposition += "\r\nprivate" }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result := createDownloadResponseFixture()
			scenario.edit(result)
			data, err := NewCreateDownloadData(result)
			if data != nil || !errors.Is(err, domain.ErrInvalidDownloadAuthorizationData) || strings.Contains(err.Error(), "private") {
				t.Fatal("异常授权结果未返回固定错误", err)
			}
		})
	}
}
