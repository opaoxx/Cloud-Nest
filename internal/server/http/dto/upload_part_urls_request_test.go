package dto_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

const partURLsFixtureSHA = "aB99887766554433aB99887766554433aB99887766554433aB99887766554433"

func TestParseUploadPartURLsRequestAcceptsBatchInOrder(t *testing.T) {
	var items []string
	for index := range domain.MaxUploadPartSigningInputs {
		number := domain.MaxUploadPartCount - index
		if index == domain.MaxUploadPartSigningInputs-1 {
			number = 1
		}
		items = append(items, fmt.Sprintf(`{"part_number":%d,"sha256":%q}`, number, partURLsFixtureSHA))
	}
	request := decodePartURLsFixture(t, `{"parts":[`+strings.Join(items, ",")+`]}`)
	parsed, fieldError := dto.ParseUploadPartURLsRequest(request)
	if fieldError != nil || parsed == nil || parsed.ExpectedExpiry != nil || len(parsed.Parts) != domain.MaxUploadPartSigningInputs {
		t.Fatal("批量上限或省略续期时间解析错误", fieldError)
	}
	for index, part := range parsed.Parts {
		wantNumber := uint32(domain.MaxUploadPartCount - index)
		if index == len(parsed.Parts)-1 {
			wantNumber = 1
		}
		if part.PartNumber != wantNumber || part.SHA256 != partURLsFixtureSHA {
			t.Fatal("分片声明的原顺序或摘要有误", index)
		}
	}

	// DTO 通过绝对范围校验后，服务仍按具体任务的计划限制编号。
	const partSize = 16 << 20
	_, err := service.PrepareUploadPartSigningInputs(partSize+1, partSize, 2, parsed.Parts)
	var declarationError *service.UploadPartDeclarationError
	if !errors.As(err, &declarationError) || declarationError.Field != "parts.0.part_number" {
		t.Fatal("服务没有按任务计划复核 DTO 交付的编号", err)
	}
}

func TestParseUploadPartURLsRequestRejectsInvalidParts(t *testing.T) {
	first := fmt.Sprintf(`{"part_number":1,"sha256":%q}`, partURLsFixtureSHA)
	for _, test := range []struct {
		name, body, field string
	}{
		{"missing batch", `{}`, "parts"},
		{"null batch", `{"parts":null}`, "parts"},
		{"empty batch", `{"parts":[]}`, "parts"},
		{"batch above limit", `{"parts":[` + strings.Repeat(first+",", domain.MaxUploadPartSigningInputs) + first + `]}`, "parts"},
		{"duplicate number", `{"parts":[` + first + "," + first + `]}`, "parts"},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkPartURLsFieldError(t, test.body, test.field)
		})
	}
	for _, number := range []string{"null", "0", "-1", "10001", "4294967296", "1.5", "1e1", `"2"`, "false", "{}"} {
		t.Run("invalid number "+number, func(t *testing.T) {
			item := fmt.Sprintf(`{"part_number":%s,"sha256":%q}`, number, partURLsFixtureSHA)
			checkPartURLsFieldError(t, `{"parts":[`+first+","+item+`]}`, "parts.1.part_number")
		})
	}
	for _, digest := range []string{"null", "42", `""`, fmt.Sprintf("%q", partURLsFixtureSHA[:63]),
		fmt.Sprintf("%q", partURLsFixtureSHA+"0"), fmt.Sprintf("%q", partURLsFixtureSHA[:63]+"g"),
		fmt.Sprintf("%q", partURLsFixtureSHA[:63]+"\n"),
	} {
		t.Run("invalid digest "+digest, func(t *testing.T) {
			item := `{"part_number":2,"sha256":` + digest + "}"
			checkPartURLsFieldError(t, `{"parts":[`+first+","+item+`]}`, "parts.1.sha256")
		})
	}
	checkPartURLsFieldError(t, `{"parts":[null]}`, "parts.0.part_number")
	checkPartURLsFieldError(t, `{"parts":[{"sha256":"`+partURLsFixtureSHA+`"}]}`, "parts.0.part_number")
	checkPartURLsFieldError(t, `{"parts":[{"part_number":2}]}`, "parts.0.sha256")
}

func TestParseUploadPartURLsRequestRenewalTime(t *testing.T) {
	prefix := fmt.Sprintf(`{"parts":[{"part_number":1,"sha256":%q}],"renew_if_expires_at":`, partURLsFixtureSHA)
	for _, test := range []struct {
		value       string
		nanoseconds int
	}{
		{`"2028-02-29T04:05:06Z"`, 0},
		{`"2028-02-29T04:05:06.1Z"`, 100_000_000},
		{`"2028-02-29T04:05:06.123456Z"`, 123_456_000},
	} {
		t.Run(test.value, func(t *testing.T) {
			parsed, fieldError := dto.ParseUploadPartURLsRequest(decodePartURLsFixture(t, prefix+test.value+"}"))
			want := time.Date(2028, 2, 29, 4, 5, 6, test.nanoseconds, time.UTC)
			if fieldError != nil || parsed == nil || parsed.ExpectedExpiry == nil || *parsed.ExpectedExpiry != want {
				t.Fatal("旧期限解析没有保留 UTC 或微秒值", fieldError)
			}
		})
	}
	for _, value := range []string{
		"null", "42", "false", "{}", `""`, `"2028-02-29T04:05:06.1234560Z"`, `"2028-02-29T04:05:06+00:00"`,
		`"2028-02-29T04:05:06z"`, `"2028-02-29T04:05:06,1Z"`, `"2028-02-29T04:05:06.Z"`, `"2028-02-29T4:05:06Z"`,
		`"2028-02-29T04:05:06Z\n"`, `"2027-02-29T04:05:06Z"`, `"2028-02-29T24:05:06Z"`,
	} {
		t.Run("invalid "+value, func(t *testing.T) {
			checkPartURLsFieldError(t, prefix+value+"}", "renew_if_expires_at")
		})
	}
}

func TestParseUploadPartURLsRequestSharedJSONBinding(t *testing.T) {
	valid := fmt.Sprintf(`{"parts":[{"part_number":1,"sha256":%q}]}`, partURLsFixtureSHA)
	for _, body := range []string{
		valid,
		strings.Replace(valid, `"part_number":1`, `"part_number":1,"unknown":true`, 1),
		strings.Replace(valid, `"parts"`, `"unknown":true,"parts"`, 1),
		valid + `{}`,
		`{"parts":{}}`,
	} {
		recorder := httptest.NewRecorder()
		requestContext, _ := gin.CreateTestContext(recorder)
		requestContext.Request = httptest.NewRequest(http.MethodPost, "/fixture", strings.NewReader(body))
		requestContext.Request.Header.Set("Content-Type", "application/json")
		var request dto.UploadPartURLsRequest
		bound := binding.ReadJSON(requestContext, &request)
		if body == valid {
			input, fieldError := dto.ParseUploadPartURLsRequest(request)
			if !bound || input == nil || fieldError != nil {
				t.Fatal("公共 JSON 绑定未交付合法请求", fieldError)
			}
		} else if bound || recorder.Code != http.StatusBadRequest {
			t.Fatal("公共 JSON 绑定未拒绝错误结构", recorder.Code)
		}
	}
}

func decodePartURLsFixture(t *testing.T, body string) dto.UploadPartURLsRequest {
	t.Helper()
	var request dto.UploadPartURLsRequest
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatal("测试夹具无法绑定", err)
	}
	return request
}

func checkPartURLsFieldError(t *testing.T, body, field string) {
	t.Helper()
	input, fieldError := dto.ParseUploadPartURLsRequest(decodePartURLsFixture(t, body))
	if input != nil || fieldError == nil || fieldError.Field != field || fieldError.Message == "" {
		t.Fatal("无效请求应只返回固定字段错误", field, fieldError)
	}
}
