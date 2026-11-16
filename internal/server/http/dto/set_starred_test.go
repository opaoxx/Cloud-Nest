package dto

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestParseSetStarredRequestResults(t *testing.T) {
	for _, starred := range []bool{true, false} {
		request := SetStarredRequest{Starred: &starred}
		input, fieldError := ParseSetStarredRequest(request)
		if fieldError != nil || input == nil || input.Starred != starred || request.Starred == nil || *request.Starred != starred {
			t.Fatal("显式收藏状态解析错误", fieldError)
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(encoded, &fields) != nil || len(fields) != 1 || string(fields["starred"]) != stringMustJSON(t, starred) {
			t.Fatal("请求布尔字段编码错误")
		}
	}
}

func TestParseSetStarredRequestMissingAndNull(t *testing.T) {
	for _, body := range []string{`{}`, `{"starred":null}`} {
		var request SetStarredRequest
		if err := json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		input, fieldError := ParseSetStarredRequest(request)
		if input != nil || fieldError == nil || fieldError.Field != "starred" || fieldError.Message != "收藏状态须明确为 true 或 false" {
			t.Fatal("缺失或 null 未定位到收藏状态", fieldError)
		}
	}
}

func TestParseSetStarredRequestIndependent(t *testing.T) {
	starred := false
	request := SetStarredRequest{Starred: &starred}
	first, firstError := ParseSetStarredRequest(request)
	second, secondError := ParseSetStarredRequest(request)
	if firstError != nil || secondError != nil || first == nil || second == nil || first == second {
		t.Fatal("解析结果未独立持有")
	}
	starred = true
	if first.Starred || second.Starred {
		t.Fatal("输入指针修改影响已转换结果")
	}
	first.Starred = true
	if second.Starred || !*request.Starred {
		t.Fatal("结果之间或结果与请求发生混用")
	}
	_, errorOne := ParseSetStarredRequest(SetStarredRequest{})
	_, errorTwo := ParseSetStarredRequest(SetStarredRequest{})
	if errorOne == nil || errorTwo == nil || errorOne == errorTwo {
		t.Fatal("错误结果未独立持有")
	}
	errorOne.Message = "本地修改"
	if errorTwo.Message != "收藏状态须明确为 true 或 false" {
		t.Fatal("修改错误影响其他调用")
	}
}

func TestParseSetStarredRequestConcurrent(t *testing.T) {
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func(starred bool) {
			defer workers.Done()
			request := SetStarredRequest{Starred: &starred}
			for attempt := 0; attempt < 20; attempt++ {
				input, fieldError := ParseSetStarredRequest(request)
				if fieldError != nil || input == nil || input.Starred != starred {
					t.Error("并发收藏解析结果错误", fieldError)
					return
				}
			}
		}(worker%2 == 0)
	}
	workers.Wait()
}
