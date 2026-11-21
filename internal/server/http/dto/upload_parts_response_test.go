package dto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const uploadPartsResponseFixtureSize = 16 << 20
const uploadPartsResponseFixtureChecksum = "TgdAhWK+24tgzgXB3s/jrRa3IjCWfeAfZAt+Rym0n84="

func uploadPartsResponseFixture() *domain.UploadPartReceiptList {
	return &domain.UploadPartReceiptList{PartSizeBytes: uploadPartsResponseFixtureSize, PartCount: 3, Parts: []domain.UploadPartReceipt{
		{PartNumber: 1, SizeBytes: uploadPartsResponseFixtureSize, ETag: "FiRsT-Tag:_1", ChecksumSHA256: uploadPartsResponseFixtureChecksum},
		{PartNumber: 3, SizeBytes: 1, ETag: "TaIl.3", ChecksumSHA256: uploadPartsResponseFixtureChecksum},
	}}
}

func TestNewUploadPartsDataJSON(t *testing.T) {
	result := uploadPartsResponseFixture()
	data, err := NewUploadPartsData(result)
	if err != nil || data == nil {
		t.Fatal("无法转换合法回执", err)
	}

	// 转换结果独立保存原值，允许清单中缺少第 2 片。
	clear(result.Parts)
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	encoded, err := json.Marshal(UploadPartsResponse{Data: *data, RequestID: requestID})
	if err != nil {
		t.Fatal("回执响应无法编码", err)
	}
	want := map[string]any{
		"request_id": requestID,
		"data": map[string]any{
			"part_size_bytes": float64(uploadPartsResponseFixtureSize),
			"part_count":      float64(3),
			"parts": []any{
				map[string]any{"part_number": float64(1), "size_bytes": float64(uploadPartsResponseFixtureSize),
					"etag": "FiRsT-Tag:_1", "checksum_sha256": uploadPartsResponseFixtureChecksum},
				map[string]any{"part_number": float64(3), "size_bytes": float64(1),
					"etag": "TaIl.3", "checksum_sha256": uploadPartsResponseFixtureChecksum},
			},
		},
	}
	var actual map[string]any
	if err := json.Unmarshal(encoded, &actual); err != nil || !reflect.DeepEqual(actual, want) {
		t.Fatal("回执的公开字段、类型、顺序或原值偏离契约", err)
	}
}

func TestNewUploadPartsDataEmptyArrays(t *testing.T) {
	for _, empty := range [][]domain.UploadPartReceipt{nil, {}} {
		result := &domain.UploadPartReceiptList{PartSizeBytes: minUploadPartsResponsePartSize, PartCount: 1, Parts: empty}
		data, err := NewUploadPartsData(result)
		if err != nil || data == nil || data.Parts == nil {
			t.Fatal("空清单未保留固定分片计划和空数组", err)
		}
		encoded, err := json.Marshal(data)
		if err != nil || string(encoded) != `{"part_size_bytes":5242880,"part_count":1,"parts":[]}` {
			t.Fatal("空清单 JSON 偏离契约", err)
		}
	}
}

func TestNewUploadPartsDataNumericBoundaries(t *testing.T) {
	result := &domain.UploadPartReceiptList{PartSizeBytes: maxJSONSafeInteger, PartCount: domain.MaxUploadPartCount,
		Parts: []domain.UploadPartReceipt{
			{PartNumber: 1, SizeBytes: maxJSONSafeInteger, ETag: strings.Repeat("A", 64), ChecksumSHA256: uploadPartsResponseFixtureChecksum},
			{PartNumber: domain.MaxUploadPartCount, SizeBytes: maxJSONSafeInteger, ETag: "FullTail", ChecksumSHA256: uploadPartsResponseFixtureChecksum},
		}}
	data, err := NewUploadPartsData(result)
	if err != nil || data == nil {
		t.Fatal("最大安全整数或最高分片编号未通过", err)
	}
	encoded, err := json.Marshal(data)
	if err != nil || !strings.Contains(string(encoded), `"part_size_bytes":9007199254740991`) ||
		!strings.Contains(string(encoded), `"part_number":10000,"size_bytes":9007199254740991`) {
		t.Fatal("最大安全整数或最高分片编号编码失真", err)
	}
}

func TestNewUploadPartsDataRejectsInvalidResult(t *testing.T) {
	if data, err := NewUploadPartsData(nil); data != nil || err != domain.ErrInvalidUploadPartReceiptListData {
		t.Fatal("未拒绝空结果")
	}
	for _, test := range []struct {
		name   string
		change func(*domain.UploadPartReceiptList)
	}{
		{"standard size below wire minimum", func(result *domain.UploadPartReceiptList) { result.PartSizeBytes = minUploadPartsResponsePartSize - 1 }},
		{"standard size exceeds safe integer", func(result *domain.UploadPartReceiptList) { result.PartSizeBytes = maxJSONSafeInteger + 1 }},
		{"zero part count", func(result *domain.UploadPartReceiptList) { result.PartCount = 0 }},
		{"oversized part count", func(result *domain.UploadPartReceiptList) { result.PartCount = domain.MaxUploadPartCount + 1 }},
		{"too many receipts", func(result *domain.UploadPartReceiptList) { result.Parts = append(result.Parts, result.Parts...) }},
		{"zero part number", func(result *domain.UploadPartReceiptList) { result.Parts[1].PartNumber = 0 }},
		{"number outside plan", func(result *domain.UploadPartReceiptList) { result.Parts[1].PartNumber = 4 }},
		{"duplicate number", func(result *domain.UploadPartReceiptList) { result.Parts[1].PartNumber = 1 }},
		{"descending number", func(result *domain.UploadPartReceiptList) {
			result.Parts[0], result.Parts[1] = result.Parts[1], result.Parts[0]
		}},
		{"zero tail bytes", func(result *domain.UploadPartReceiptList) { result.Parts[1].SizeBytes = 0 }},
		{"oversized tail", func(result *domain.UploadPartReceiptList) { result.Parts[1].SizeBytes = result.PartSizeBytes + 1 }},
		{"short intermediate part", func(result *domain.UploadPartReceiptList) { result.Parts[0].SizeBytes-- }},
		{"missing ETag", func(result *domain.UploadPartReceiptList) { result.Parts[1].ETag = "" }},
		{"quoted ETag", func(result *domain.UploadPartReceiptList) { result.Parts[1].ETag = `"private-etag"` }},
		{"oversized ETag", func(result *domain.UploadPartReceiptList) { result.Parts[1].ETag = strings.Repeat("x", 65) }},
		{"missing SHA-256", func(result *domain.UploadPartReceiptList) { result.Parts[1].ChecksumSHA256 = "" }},
		{"unusable SHA-256", func(result *domain.UploadPartReceiptList) {
			result.Parts[1].ChecksumSHA256 = strings.Repeat("A", 42) + "B="
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := uploadPartsResponseFixture()
			test.change(result)
			if data, err := NewUploadPartsData(result); data != nil || err != domain.ErrInvalidUploadPartReceiptListData {
				t.Fatal("异常回执应使整批只返回固定错误")
			}
		})
	}
}
