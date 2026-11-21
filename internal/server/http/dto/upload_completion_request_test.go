package dto_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestParseCompleteMultipartUploadRequestPreservesDeclarations(t *testing.T) {
	// 1. 提交跳号且乱序的回执，格式校验保留原顺序和大小写。
	body := `{"parts":[{"part_number":3,"etag":"Ab-z_9.:K"},{"part_number":1,"etag":"F"}]}`
	request, bound, status := bindCompletionRequestFixture(body)
	if !bound {
		t.Fatal("有效完成请求绑定失败", status)
	}
	claims, fieldError := dto.ParseCompleteMultipartUploadRequest(request)
	want := []domain.UploadPartCompletionClaim{{PartNumber: 3, ETag: "Ab-z_9.:K"}, {PartNumber: 1, ETag: "F"}}
	if fieldError != nil || !slices.Equal(claims, want) {
		t.Fatalf("完成声明的顺序、编号或 ETag 不符：%v，%v", claims, fieldError)
	}
}

func TestParseCompleteMultipartUploadRequestMaximumBatch(t *testing.T) {
	// 1. 走共享 JSON 绑定，确认标准紧凑正文容纳 10000 个最长 ETag。
	etag := strings.Repeat("A", 64)
	var body strings.Builder
	body.WriteString(`{"parts":[`)
	for partNumber := domain.MaxUploadPartCount; partNumber >= 1; partNumber-- {
		if partNumber != domain.MaxUploadPartCount {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `{"part_number":%d,"etag":%q}`, partNumber, etag)
	}
	body.WriteString("]}")
	if int64(body.Len()) > binding.MaxJSONBodyBytes {
		t.Fatal("最大批次的紧凑正文超出既有上限", body.Len())
	}
	request, bound, status := bindCompletionRequestFixture(body.String())
	if !bound {
		t.Fatal("最大批次绑定失败", status)
	}
	claims, fieldError := dto.ParseCompleteMultipartUploadRequest(request)
	if fieldError != nil || len(claims) != domain.MaxUploadPartCount {
		t.Fatal("最大批次解析失败", fieldError)
	}
	for index, claim := range claims {
		if claim.PartNumber != uint32(domain.MaxUploadPartCount-index) || claim.ETag != etag {
			t.Fatal("最大批次转换后内容不符", index)
		}
	}
	t.Logf("紧凑正文 %d 字节，分片 %d 项，每项 ETag 64 字符", body.Len(), len(claims))
}

func TestParseCompleteMultipartUploadRequestFieldErrors(t *testing.T) {
	const first = `{"part_number":1,"etag":"Valid"}`
	type fieldCase struct{ name, body, field string }
	tests := []fieldCase{
		{"missing batch", `{}`, "parts"},
		{"null batch", `{"parts":null}`, "parts"},
		{"empty batch", `{"parts":[]}`, "parts"},
		{"oversized batch", `{"parts":[` + strings.Repeat(first+",", domain.MaxUploadPartCount) + first + `]}`, "parts"},
		{"duplicate nonadjacent number", `{"parts":[` + first + `,{"part_number":2,"etag":"Other"},{"part_number":1,"etag":"Changed"}]}`, "parts"},
		{"null item", `{"parts":[null]}`, "parts.0.part_number"},
		{"missing number", `{"parts":[{"etag":"Valid"}]}`, "parts.0.part_number"},
		{"missing ETag", `{"parts":[{"part_number":1}]}`, "parts.0.etag"},
	}
	for _, number := range []string{"null", "0", "-1", "10001", "4294967296", "2.5", "2e0", `"2"`, "false", "{}"} {
		tests = append(tests, fieldCase{
			"invalid number " + number, `{"parts":[` + first + `,{"part_number":` + number + `,"etag":"Valid"}]}`, "parts.1.part_number",
		})
	}
	for _, etag := range []string{"null", "42", `""`, `" First"`, `"a/b"`, `"\"First\""`, `"First\n"`,
		`"样例"`, `"` + strings.Repeat("A", 65) + `"`} {
		tests = append(tests, fieldCase{
			"invalid ETag " + etag, `{"parts":[` + first + `,{"part_number":2,"etag":` + etag + `}]}`, "parts.1.etag",
		})
	}
	// 1. 字段错误指向原数组下标，已有合法项也不会被部分交付。
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, bound, status := bindCompletionRequestFixture(test.body)
			if !bound {
				t.Fatal("字段校验夹具的 JSON 绑定失败", status)
			}
			claims, fieldError := dto.ParseCompleteMultipartUploadRequest(request)
			if claims != nil || fieldError == nil || fieldError.Field != test.field || fieldError.Message == "" {
				t.Fatalf("应返回固定字段错误 %q，得到 %v", test.field, fieldError)
			}
		})
	}
}

func TestParseCompleteMultipartUploadRequestSharedJSONBinding(t *testing.T) {
	for _, body := range []string{
		`{"parts":[{"part_number":1,"etag":"Valid","size_bytes":10}]}`,
		`{"parts":[{"part_number":1,"etag":"Valid"}],"unknown":true}`,
		`{"parts":[{"part_number":1,"etag":"Valid"}]} {}`,
		`{"parts":{}}`,
		`{"parts":[1]}`,
	} {
		_, bound, status := bindCompletionRequestFixture(body)
		if bound || status != http.StatusBadRequest {
			t.Fatalf("共享 JSON 绑定应拒绝错误结构 %q，得到 %d", body, status)
		}
	}
}

func bindCompletionRequestFixture(body string) (dto.CompleteMultipartUploadRequest, bool, int) {
	response := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(response)
	requestContext.Request = httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(body))
	requestContext.Request.Header.Set("Content-Type", "application/json")
	var request dto.CompleteMultipartUploadRequest
	bound := binding.ReadJSON(requestContext, &request)
	return request, bound, response.Code
}
