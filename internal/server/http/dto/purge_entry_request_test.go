package dto

import (
	"bufio"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestParsePurgeEntryHeaders(t *testing.T) {
	maximumDigest := strings.Repeat("A", domain.MaxPurgePreviewDigestLength-len(purgePreviewExampleDigest)) + purgePreviewExampleDigest
	for _, scenario := range []struct {
		name, revision, digest string
		wantRevision           uint64
	}{
		{"example", "4", purgePreviewExampleDigest, 4},
		{"minimum revision", "1", purgePreviewExampleDigest, 1},
		{"maximum revision and credential length", "18446744073709551615", maximumDigest, math.MaxUint64},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 通过标准 HTTP 头方法设置单值，核对完整版本与原样凭证。
			headers := make(http.Header)
			headers.Set(PurgeExpectedRevisionHeader, scenario.revision)
			headers.Set(PurgePreviewDigestHeader, scenario.digest)
			input, fieldError := ParsePurgeEntryHeaders(headers)
			if fieldError != nil || input == nil || input.ExpectedRevision != scenario.wantRevision || input.PreviewDigest != scenario.digest {
				t.Fatal("确认输入没有保留完整版本或凭证", fieldError)
			}
			headers.Set(PurgeExpectedRevisionHeader, "7")
			headers.Set(PurgePreviewDigestHeader, "")
			if input.ExpectedRevision != scenario.wantRevision || input.PreviewDigest != scenario.digest {
				t.Fatal("解析得到的确认输入随原请求头改变")
			}
		})
	}
}

func TestParsePurgeEntryHeadersRejectsInvalidValues(t *testing.T) {
	for _, scenario := range []struct {
		name string
		edit func(http.Header)
	}{
		{"missing version", func(headers http.Header) { headers.Del(PurgeExpectedRevisionHeader) }},
		{"empty version", func(headers http.Header) { headers.Set(PurgeExpectedRevisionHeader, "") }},
		{"duplicate version", func(headers http.Header) { headers.Add(PurgeExpectedRevisionHeader, "4") }},
		{"merged version", func(headers http.Header) { headers.Set(PurgeExpectedRevisionHeader, "4,4") }},
		{"zero version", func(headers http.Header) { headers.Set(PurgeExpectedRevisionHeader, "0") }},
		{"leading zero", func(headers http.Header) { headers.Set(PurgeExpectedRevisionHeader, "04") }},
		{"signed version", func(headers http.Header) { headers.Set(PurgeExpectedRevisionHeader, "+4") }},
		{"space in parsed value", func(headers http.Header) { headers.Set(PurgeExpectedRevisionHeader, "4 ") }},
		{"uint64 overflow", func(headers http.Header) { headers.Set(PurgeExpectedRevisionHeader, "18446744073709551616") }},
		{"version error first", func(headers http.Header) { clear(headers) }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			headers := make(http.Header)
			headers.Set(PurgeExpectedRevisionHeader, "4")
			headers.Set(PurgePreviewDigestHeader, purgePreviewExampleDigest)
			scenario.edit(headers)
			input, fieldError := ParsePurgeEntryHeaders(headers)
			want := FieldError{Field: PurgeExpectedRevisionHeader, Message: "预期版本须为单个 1～18446744073709551615 的标准十进制整数"}
			if input != nil || fieldError == nil || *fieldError != want {
				t.Fatal("异常版本应返回固定的版本头字段错误", fieldError)
			}
		})
	}

	for _, scenario := range []struct {
		name   string
		values []string
	}{
		{"missing credential", nil},
		{"empty credential", []string{""}},
		{"duplicate credential", []string{purgePreviewExampleDigest, purgePreviewExampleDigest}},
		{"merged credential", []string{purgePreviewExampleDigest + "," + purgePreviewExampleDigest}},
		{"wrong separator", []string{strings.Replace(purgePreviewExampleDigest, ".", ":", 1)}},
		{"extra separator", []string{purgePreviewExampleDigest + "."}},
		{"short signature", []string{purgePreviewExampleDigest[:len(purgePreviewExampleDigest)-1]}},
		{"padding", []string{purgePreviewExampleDigest + "="}},
		{"space in parsed value", []string{" " + purgePreviewExampleDigest}},
		{"newline", []string{purgePreviewExampleDigest + "\n"}},
		{"non-ASCII", []string{"凭证" + purgePreviewExampleDigest}},
		{"credential too long", []string{strings.Repeat("a", domain.MaxPurgePreviewDigestLength+1-len(purgePreviewExampleDigest)) + purgePreviewExampleDigest}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			headers := make(http.Header)
			headers.Set(PurgeExpectedRevisionHeader, "4")
			for _, value := range scenario.values {
				headers.Add(PurgePreviewDigestHeader, value)
			}
			input, fieldError := ParsePurgeEntryHeaders(headers)
			want := FieldError{Field: PurgePreviewDigestHeader, Message: domain.ErrInvalidPurgePreviewDigest.Error()}
			if input != nil || fieldError == nil || *fieldError != want {
				t.Fatal("异常凭证应返回固定的凭证头字段错误", fieldError)
			}
		})
	}
}

func TestParsePurgeEntryHeadersTransport(t *testing.T) {
	// 1. 从 HTTP 字节解析不同大小写的头名，并保留重复头行。
	const start = "DELETE /api/v1/trash/23?expected_revision=9&preview_digest=ignored HTTP/1.1\r\nHost: example.test\r\n"
	const upperRevision = "X-CLOUDNEST-EXPECTED-REVISION: 4\r\n"
	const lowerDigest = "x-cloudnest-purge-preview: " + purgePreviewExampleDigest + "\r\n"
	for _, scenario := range []struct {
		name, headers, invalidField string
	}{
		{"different casing", upperRevision + lowerDigest, ""},
		{"duplicate version with different casing", upperRevision + "x-cloudnest-expected-revision: 4\r\n" + lowerDigest, PurgeExpectedRevisionHeader},
		{"duplicate credential with different casing", upperRevision + lowerDigest + "X-CloudNest-Purge-Preview: " + purgePreviewExampleDigest + "\r\n", PurgePreviewDigestHeader},
		{"query cannot replace version", lowerDigest, PurgeExpectedRevisionHeader},
		{"query cannot replace credential", upperRevision, PurgePreviewDigestHeader},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			request, err := http.ReadRequest(bufio.NewReader(strings.NewReader(start + scenario.headers + "\r\n")))
			if err != nil {
				t.Fatal(err)
			}
			input, fieldError := ParsePurgeEntryHeaders(request.Header)
			if scenario.invalidField != "" {
				if input != nil || fieldError == nil || fieldError.Field != scenario.invalidField {
					t.Fatal("HTTP 请求未定位到错误的头", fieldError)
				}
				return
			}
			if fieldError != nil || input == nil || input.ExpectedRevision != 4 || input.PreviewDigest != purgePreviewExampleDigest {
				t.Fatal("HTTP 请求头没有解析出应有的版本和凭证", fieldError)
			}
		})
	}
}
