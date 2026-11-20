package dto

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestParseCreateDownloadRequestValid(t *testing.T) {
	// 1. 从实际 JSON 解码，核对两个用途和完整版本范围。
	for _, scenario := range []struct {
		name, body, purpose string
		revision            uint64
	}{
		{"minimum version", `{"expected_revision":"1","purpose":"download"}`, "download", 1},
		{"beyond JavaScript safe integer", `{"expected_revision":"9007199254740993","purpose":"preview"}`, "preview", 9007199254740993},
		{"maximum version", `{"expected_revision":"18446744073709551615","purpose":"download"}`, "download", math.MaxUint64},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var request CreateDownloadRequest
			if err := json.Unmarshal([]byte(scenario.body), &request); err != nil {
				t.Fatal(err)
			}
			input, fieldError := ParseCreateDownloadRequest(request)
			if fieldError != nil || input == nil || input.ExpectedRevision != scenario.revision || input.Purpose != scenario.purpose {
				t.Fatal("未保留预期版本或用途", input, fieldError)
			}
			encoded, err := json.Marshal(request)
			if err != nil || string(encoded) != scenario.body {
				t.Fatal("解析修改了请求或 JSON 字段不符", string(encoded), err)
			}
		})
	}
}

func TestParseCreateDownloadRequestFailures(t *testing.T) {
	// 1. 非标准版本先返回版本字段错误，即使同时缺少用途。
	const versionMessage = "预期版本须为 1～18446744073709551615 的标准十进制整数"
	for _, version := range []string{"", "0", "03", "+3", "-3", " 3", "3 ", "3\n", "3.0", "3e1", "３", "18446744073709551616", strings.Repeat("9", 100)} {
		t.Run("version "+version, func(t *testing.T) {
			input, fieldError := ParseCreateDownloadRequest(CreateDownloadRequest{ExpectedRevision: &version})
			if input != nil || fieldError == nil || fieldError.Field != "expected_revision" || fieldError.Message != versionMessage {
				t.Fatal("非法版本未返回固定字段错误", input, fieldError)
			}
		})
	}

	// 2. 版本通过后，用途精确匹配小写值。
	const purposeMessage = "下载授权用途须明确为 download 或 preview"
	version := "3"
	for _, purpose := range []string{"", "open", "Download", "PREVIEW", " preview", "download ", "preview\n", "预览", "download\x00"} {
		t.Run("purpose "+purpose, func(t *testing.T) {
			input, fieldError := ParseCreateDownloadRequest(CreateDownloadRequest{ExpectedRevision: &version, Purpose: &purpose})
			if input != nil || fieldError == nil || fieldError.Field != "purpose" || fieldError.Message != purposeMessage {
				t.Fatal("非法用途未返回固定字段错误", input, fieldError)
			}
		})
	}

	// 3. 缺失和显式 null 都由 DTO 定位到对应字段。
	for _, scenario := range []struct{ body, field, message string }{
		{`{}`, "expected_revision", versionMessage},
		{`{"purpose":"download"}`, "expected_revision", versionMessage},
		{`{"expected_revision":null,"purpose":"preview"}`, "expected_revision", versionMessage},
		{`{"expected_revision":"3","expected_revision":null,"purpose":"preview"}`, "expected_revision", versionMessage},
		{`{"expected_revision":"3"}`, "purpose", purposeMessage},
		{`{"expected_revision":"3","purpose":null}`, "purpose", purposeMessage},
		{`{"expected_revision":"3","purpose":"download","purpose":null}`, "purpose", purposeMessage},
	} {
		t.Run(scenario.body, func(t *testing.T) {
			var request CreateDownloadRequest
			if err := json.Unmarshal([]byte(scenario.body), &request); err != nil {
				t.Fatal(err)
			}
			input, fieldError := ParseCreateDownloadRequest(request)
			if input != nil || fieldError == nil || fieldError.Field != scenario.field || fieldError.Message != scenario.message {
				t.Fatal("缺失或空字段定位错误", input, fieldError)
			}
		})
	}
}

func TestCreateDownloadRequestRejectsWrongJSONTypes(t *testing.T) {
	for _, body := range []string{
		`{"expected_revision":3,"purpose":"download"}`,
		`{"expected_revision":{},"purpose":"preview"}`,
		`{"expected_revision":"3","purpose":true}`,
		`{"expected_revision":"3","purpose":[]}`,
	} {
		var request CreateDownloadRequest
		if err := json.Unmarshal([]byte(body), &request); err == nil {
			t.Errorf("错误 JSON 字段类型被读入请求结构体 %s", body)
		}
	}
}

func TestParseCreateDownloadRequestIndependent(t *testing.T) {
	version, purpose := "3", "download"
	request := CreateDownloadRequest{ExpectedRevision: &version, Purpose: &purpose}
	first, firstError := ParseCreateDownloadRequest(request)
	second, secondError := ParseCreateDownloadRequest(request)
	if firstError != nil || secondError != nil || first == nil || second == nil || first == second {
		t.Fatal("每次解析须生成独立参数", firstError, secondError)
	}
	version, purpose = "4", "preview"
	first.ExpectedRevision, first.Purpose = 5, "preview"
	if second.ExpectedRevision != 3 || second.Purpose != "download" {
		t.Fatal("修改输入或另一个结果影响已校验参数", second)
	}
}
