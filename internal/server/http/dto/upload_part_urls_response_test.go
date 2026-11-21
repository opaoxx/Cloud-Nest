package dto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const uploadPartURLResponseChecksum = "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="

func uploadPartURLsResponseFixture() *domain.UploadPartURLBatch {
	taskExpiry := time.Date(2026, 11, 21, 20, 10, 0, 123456789, time.FixedZone("UTC+8", 8*3600))
	parts := []domain.UploadPartURL{
		{
			PartNumber: 2, SizeBytes: 1, ChecksumSHA256: uploadPartURLResponseChecksum,
			URL:          "https://storage.example.invalid/bucket/chunk%2B.bin?partNumber=2&uploadId=a%2B%2F%3D&X-Amz-Signature=fixture2",
			URLExpiresAt: time.Date(2026, 11, 21, 8, 5, 0, 0, time.FixedZone("UTC-4", -4*3600)),
		},
		{
			PartNumber: 1, SizeBytes: 16 << 20, ChecksumSHA256: strings.Repeat("A", 43) + "=",
			URL:          "http://[::1]:19000/bucket/chunk%2B.bin?partNumber=1&uploadId=a%2B%2F%3D&X-Amz-Signature=fixture1",
			URLExpiresAt: time.Date(2026, 11, 21, 12, 5, 1, 0, time.UTC),
		},
	}
	return &domain.UploadPartURLBatch{ExpiresAt: taskExpiry, Parts: parts}
}

func TestNewUploadPartURLsDataJSON(t *testing.T) {
	result := uploadPartURLsResponseFixture()
	data, err := NewUploadPartURLsData(result)
	if err != nil || data == nil {
		t.Fatal("合法签发结果转换失败", err)
	}
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	expected := map[string]any{
		"request_id": requestID,
		"data": map[string]any{
			"expires_at": "2026-11-21T12:10:00.123456Z",
			"parts": []any{
				map[string]any{
					"part_number": float64(2), "size_bytes": float64(1), "url": result.Parts[0].URL,
					"required_headers": map[string]any{"x-amz-checksum-sha256": uploadPartURLResponseChecksum},
					"url_expires_at":   "2026-11-21T12:05:00Z",
				},
				map[string]any{
					"part_number": float64(1), "size_bytes": float64(16 << 20), "url": result.Parts[1].URL,
					"required_headers": map[string]any{"x-amz-checksum-sha256": result.Parts[1].ChecksumSHA256},
					"url_expires_at":   "2026-11-21T12:05:01Z",
				},
			},
		},
	}

	// 改动服务原结果后再编码，确认公开批次独立保留转换时的值。
	result.ExpiresAt = time.Time{}
	clear(result.Parts)
	body, err := json.Marshal(UploadPartURLsResponse{Data: *data, RequestID: requestID})
	if err != nil {
		t.Fatal("签发响应无法编码", err)
	}
	var actual map[string]any
	if err := json.Unmarshal(body, &actual); err != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatal("签发响应字段、类型、顺序或原始签名值偏离契约", err)
	}
}

func TestNewUploadPartURLsDataBoundaries(t *testing.T) {
	result := uploadPartURLsResponseFixture()
	lastPart := result.Parts[0]
	lastPart.PartNumber, lastPart.SizeBytes = domain.MaxUploadPartCount, maxJSONSafeInteger
	lastPart.URLExpiresAt = result.ExpiresAt
	result.Parts = []domain.UploadPartURL{lastPart}

	data, err := NewUploadPartURLsData(result)
	if err != nil || data == nil || len(data.Parts) != 1 || data.Parts[0].PartNumber != domain.MaxUploadPartCount ||
		data.Parts[0].SizeBytes != maxJSONSafeInteger || !data.Parts[0].URLExpiresAt.Equal(data.ExpiresAt) {
		t.Fatal("绝对编号、整数上限或期限相等边界转换错误", err)
	}
	result.Parts = make([]domain.UploadPartURL, domain.MaxUploadPartSigningInputs)
	for index := range result.Parts {
		result.Parts[index] = lastPart
		result.Parts[index].PartNumber -= uint32(index)
	}
	data, err = NewUploadPartURLsData(result)
	if err != nil || data == nil || len(data.Parts) != domain.MaxUploadPartSigningInputs {
		t.Fatal("批次上限内的结果转换失败", err)
	}
}

func TestNewUploadPartURLsDataRejectsInvalidBatch(t *testing.T) {
	if data, err := NewUploadPartURLsData(nil); data != nil || err != domain.ErrInvalidUploadPartURLBatchData {
		t.Fatal("空签发结果未被拒绝", err)
	}
	for _, test := range []struct {
		name   string
		change func(*domain.UploadPartURLBatch)
	}{
		{"empty batch", func(result *domain.UploadPartURLBatch) { result.Parts = nil }},
		{"oversized batch", func(result *domain.UploadPartURLBatch) {
			result.Parts = make([]domain.UploadPartURL, domain.MaxUploadPartSigningInputs+1)
		}},
		{"missing task expiry", func(result *domain.UploadPartURLBatch) { result.ExpiresAt = time.Time{} }},
		{"task expiry out of range", func(result *domain.UploadPartURLBatch) {
			result.ExpiresAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
		}},
		{"zero number", func(result *domain.UploadPartURLBatch) { result.Parts[1].PartNumber = 0 }},
		{"high number", func(result *domain.UploadPartURLBatch) { result.Parts[1].PartNumber = domain.MaxUploadPartCount + 1 }},
		{"duplicate number", func(result *domain.UploadPartURLBatch) { result.Parts[1].PartNumber = result.Parts[0].PartNumber }},
		{"zero bytes", func(result *domain.UploadPartURLBatch) { result.Parts[1].SizeBytes = 0 }},
		{"unsafe bytes", func(result *domain.UploadPartURLBatch) { result.Parts[1].SizeBytes = maxJSONSafeInteger + 1 }},
		{"relative URL", func(result *domain.UploadPartURLBatch) { result.Parts[1].URL = "/private?X-Amz-Signature=fixture" }},
		{"URL credentials", func(result *domain.UploadPartURLBatch) {
			result.Parts[1].URL = "https://name:secret@storage.example.invalid/private"
		}},
		{"URL fragment", func(result *domain.UploadPartURLBatch) { result.Parts[1].URL += "#private" }},
		{"missing URL expiry", func(result *domain.UploadPartURLBatch) { result.Parts[1].URLExpiresAt = time.Time{} }},
		{"URL expiry out of range", func(result *domain.UploadPartURLBatch) {
			result.Parts[1].URLExpiresAt = time.Date(999, 12, 31, 23, 59, 59, 0, time.UTC)
		}},
		{"URL expires after task", func(result *domain.UploadPartURLBatch) {
			result.Parts[1].URLExpiresAt = result.ExpiresAt.Add(time.Nanosecond)
		}},
		{"missing base64 padding", func(result *domain.UploadPartURLBatch) {
			result.Parts[1].ChecksumSHA256 = strings.TrimSuffix(uploadPartURLResponseChecksum, "=")
		}},
		{"noncanonical base64 padding bits", func(result *domain.UploadPartURLBatch) {
			result.Parts[1].ChecksumSHA256 = strings.Repeat("A", 42) + "B="
		}},
		{"wrong base64 alphabet", func(result *domain.UploadPartURLBatch) {
			result.Parts[1].ChecksumSHA256 = strings.ReplaceAll(uploadPartURLResponseChecksum, "+", "-")
		}},
		{"base64 newline", func(result *domain.UploadPartURLBatch) {
			result.Parts[1].ChecksumSHA256 = uploadPartURLResponseChecksum[:42] + "\n="
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := uploadPartURLsResponseFixture()
			test.change(result)
			if data, err := NewUploadPartURLsData(result); data != nil || err != domain.ErrInvalidUploadPartURLBatchData {
				t.Fatal("异常批次应只返回固定错误")
			}
		})
	}
}
