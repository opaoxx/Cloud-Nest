package dto

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestParseRecordAccessRequestResults(t *testing.T) {
	for _, purpose := range []string{"preview", "open"} {
		t.Run(purpose, func(t *testing.T) {
			request := RecordAccessRequest{Purpose: &purpose}
			input, fieldError := ParseRecordAccessRequest(request)
			if fieldError != nil || input == nil || input.Purpose != purpose || request.Purpose == nil || *request.Purpose != purpose {
				t.Fatal("访问用途解析或原值保护错误", fieldError)
			}
			encoded, err := json.Marshal(request)
			if err != nil || string(encoded) != `{"purpose":"`+purpose+`"}` {
				t.Fatal("访问请求字段或文本编码错误", err)
			}
		})
	}
}

func TestParseRecordAccessRequestFailures(t *testing.T) {
	for _, purpose := range []string{"", "PREVIEW", "Open", "download", " preview", "open ", "\topen", "preview\n", "open\x00", "预览", "ｏｐｅｎ", "open\xff", strings.Repeat("p", 1024)} {
		t.Run(purpose, func(t *testing.T) {
			request := RecordAccessRequest{Purpose: &purpose}
			input, fieldError := ParseRecordAccessRequest(request)
			if input != nil || fieldError == nil || fieldError.Field != "purpose" || fieldError.Message != "访问用途须明确为 preview 或 open" || *request.Purpose != purpose {
				t.Fatal("非法用途的错误位置、提示或输入原值错误", fieldError)
			}
		})
	}
	for _, body := range []string{`{}`, `{"purpose":null}`, `{"purpose":"preview","purpose":null}`} {
		t.Run(body, func(t *testing.T) {
			var request RecordAccessRequest
			if err := json.Unmarshal([]byte(body), &request); err != nil {
				t.Fatal(err)
			}
			input, fieldError := ParseRecordAccessRequest(request)
			if input != nil || fieldError == nil || fieldError.Field != "purpose" || fieldError.Message != "访问用途须明确为 preview 或 open" {
				t.Fatal("缺失或 null 用途未定位字段", fieldError)
			}
		})
	}
}

func TestParseRecordAccessRequestIndependent(t *testing.T) {
	// 1. 两次解析取得独立参数，修改来源后仍保留原用途。
	purpose := "preview"
	request := RecordAccessRequest{Purpose: &purpose}
	first, firstError := ParseRecordAccessRequest(request)
	second, secondError := ParseRecordAccessRequest(request)
	if firstError != nil || secondError != nil || first == nil || second == nil || first == second {
		t.Fatal("解析结果未独立持有")
	}
	purpose = "open"
	if first.Purpose != "preview" || second.Purpose != "preview" {
		t.Fatal("来源修改影响已解析用途")
	}
	first.Purpose = "local change"
	if second.Purpose != "preview" || *request.Purpose != "open" {
		t.Fatal("参数之间或参数与来源混用")
	}

	// 2. 每次错误独立持有固定字段与提示。
	_, errorOne := ParseRecordAccessRequest(RecordAccessRequest{})
	_, errorTwo := ParseRecordAccessRequest(RecordAccessRequest{})
	if errorOne == nil || errorTwo == nil || errorOne == errorTwo {
		t.Fatal("错误对象未独立持有")
	}
	errorOne.Field, errorOne.Message = "local field", "local message"
	if errorTwo.Field != "purpose" || errorTwo.Message != "访问用途须明确为 preview 或 open" {
		t.Fatal("修改错误影响其他调用")
	}
}

func TestParseRecordAccessRequestConcurrent(t *testing.T) {
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			purpose := "preview"
			if worker%2 == 1 {
				purpose = "open"
			}
			request := RecordAccessRequest{Purpose: &purpose}
			for attempt := 0; attempt < 20; attempt++ {
				input, fieldError := ParseRecordAccessRequest(request)
				if fieldError != nil || input == nil || input.Purpose != purpose {
					t.Error("并发用途解析错误", fieldError)
					return
				}
			}
		}(worker)
	}
	workers.Wait()
}
