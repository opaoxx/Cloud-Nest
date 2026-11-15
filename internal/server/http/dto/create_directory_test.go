package dto

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestParseCreateDirectoryRequestResults(t *testing.T) {
	green, empty, padded, maximum := "green", "", " Green-2_ ", strings.Repeat("a", 16)
	for _, testCase := range []struct {
		name, parentID, entryName string
		color                     *string
		wantParentID              uint64
	}{
		{name: "smallest parent", parentID: "1", entryName: "资料", wantParentID: 1},
		{name: "normal parent", parentID: "11", entryName: "资料", wantParentID: 11},
		{name: "above javascript safe integer", parentID: "9007199254740993", entryName: "资料", wantParentID: 9007199254740993},
		{name: "above signed integer", parentID: "9223372036854775808", entryName: "资料", wantParentID: 9223372036854775808},
		{name: "maximum parent", parentID: "18446744073709551615", entryName: "资料", wantParentID: ^uint64(0)},
		{name: "green color", parentID: "11", entryName: "资料", color: &green, wantParentID: 11},
		{name: "empty color", parentID: "11", entryName: "资料", color: &empty, wantParentID: 11},
		{name: "color preserved", parentID: "11", entryName: "资料", color: &padded, wantParentID: 11},
		{name: "maximum color length", parentID: "11", entryName: "资料", color: &maximum, wantParentID: 11},
		{name: "raw name whitespace", parentID: "11", entryName: "  My Notes 🌱  ", wantParentID: 11},
		{name: "decomposed name", parentID: "11", entryName: "Cafe\u0301", wantParentID: 11},
		{name: "blank name kept for service", parentID: "11", entryName: "   ", wantParentID: 11},
		{name: "path character kept for service", parentID: "11", entryName: "资料/一", wantParentID: 11},
		{name: "raw padding before service trim", parentID: "11", entryName: strings.Repeat(" ", 256) + "a", wantParentID: 11},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := CreateDirectoryRequest{ParentID: testCase.parentID, Name: testCase.entryName, ColorKey: testCase.color}
			input, fieldError := ParseCreateDirectoryRequest(request)
			if fieldError != nil || input == nil || input.ParentID != testCase.wantParentID || input.Name != request.Name || (input.ColorKey == nil) != (request.ColorKey == nil) {
				t.Fatal("创建目录输入转换或原文错误", fieldError)
			}
			if request.ColorKey != nil && (*input.ColorKey != *request.ColorKey || input.ColorKey == request.ColorKey) {
				t.Fatal("颜色原文或独立副本错误")
			}
			if request.ParentID != testCase.parentID || request.Name != testCase.entryName {
				t.Fatal("解析修改了请求字段")
			}
		})
	}
}

func TestParseCreateDirectoryRequestFailures(t *testing.T) {
	for _, parentID := range []string{"", "0", "00", "01", "+1", "-1", " 1", "1 ", "1\n", "1.0", "1e1", "0x10", "１", "١", "18446744073709551616", strings.Repeat("9", 100), "private-parent"} {
		input, fieldError := ParseCreateDirectoryRequest(CreateDirectoryRequest{ParentID: parentID, Name: "资料"})
		if input != nil || fieldError == nil || fieldError.Field != "parent_id" || fieldError.Message != "父目录 ID 须为 1～18446744073709551615 的标准十进制整数" {
			t.Fatal("非法父目录未返回固定字段错误", fieldError)
		}
	}
	for _, name := range []string{"", "private-name\xff", "\xc0\xaf", "\xed\xa0\x80", "\xf0\x9f"} {
		input, fieldError := ParseCreateDirectoryRequest(CreateDirectoryRequest{ParentID: "11", Name: name})
		if input != nil || fieldError == nil || fieldError.Field != "name" || fieldError.Message != "名称须为非空的有效 UTF-8 文本" {
			t.Fatal("非法名称文本未返回固定字段错误", fieldError)
		}
	}
	for _, color := range []string{strings.Repeat("a", 17), "绿色", "🌱", "green\u00a0", "\u200b", "private-color\xff"} {
		input, fieldError := ParseCreateDirectoryRequest(CreateDirectoryRequest{ParentID: "11", Name: "资料", ColorKey: &color})
		if input != nil || fieldError == nil || fieldError.Field != "color_key" || fieldError.Message != "颜色键须为 null 或不超过 16 字节的可打印 ASCII 文本" {
			t.Fatal("非法颜色未返回固定字段错误", fieldError)
		}
	}

	// 全部 ASCII 码值逐项验证，可打印字符与控制字符分别检查。
	for character := rune(0); character < 128; character++ {
		t.Run(fmt.Sprintf("color U+%04X", character), func(t *testing.T) {
			color := string(character)
			input, fieldError := ParseCreateDirectoryRequest(CreateDirectoryRequest{ParentID: "11", Name: "资料", ColorKey: &color})
			if character >= 0x20 && character <= 0x7e {
				if fieldError != nil || input == nil || input.ColorKey == nil || *input.ColorKey != color {
					t.Fatal("可打印 ASCII 颜色被错误拒绝", fieldError)
				}
			} else if input != nil || fieldError == nil || fieldError.Field != "color_key" {
				t.Fatal("ASCII 控制字符未拒绝", fieldError)
			}
		})
	}
}

func TestParseCreateDirectoryRequestOrderAndCopies(t *testing.T) {
	invalidColor := "private-color\n"
	request := CreateDirectoryRequest{ColorKey: &invalidColor}
	_, first := ParseCreateDirectoryRequest(request)
	_, second := ParseCreateDirectoryRequest(request)
	if first == nil || second == nil || first == second || first.Field != "parent_id" || second.Field != "parent_id" {
		t.Fatal("首个错误顺序或独立错误对象错误")
	}
	first.Field, first.Message = "changed", "changed"
	if second.Field != "parent_id" || strings.Contains(second.Message, "changed") {
		t.Fatal("字段错误共享了可变状态")
	}
	request.ParentID = "11"
	if input, fieldError := ParseCreateDirectoryRequest(request); input != nil || fieldError == nil || fieldError.Field != "name" {
		t.Fatal("名称检查未先于颜色检查")
	}
	request.Name = "资料"
	if input, fieldError := ParseCreateDirectoryRequest(request); input != nil || fieldError == nil || fieldError.Field != "color_key" {
		t.Fatal("颜色检查顺序错误")
	}
	color := "green"
	request.ColorKey = &color
	firstInput, firstError := ParseCreateDirectoryRequest(request)
	secondInput, secondError := ParseCreateDirectoryRequest(request)
	if firstError != nil || secondError != nil || firstInput == secondInput || firstInput.ColorKey == secondInput.ColorKey {
		t.Fatal("输入结果未独立分配")
	}
	*firstInput.ColorKey, color = "sand", "blue"
	if *secondInput.ColorKey != "green" || request.ParentID != "11" || secondInput.Name != "资料" {
		t.Fatal("颜色修改影响了其他已解析输入")
	}
}

func TestCreateDirectoryRequestJSON(t *testing.T) {
	for _, body := range []string{
		`{"parent_id":"18446744073709551615","name":"  Cafe\u0301 🌱  "}`,
		`{"parent_id":"18446744073709551615","name":"  Cafe\u0301 🌱  ","color_key":null}`,
		`{"parent_id":"18446744073709551615","name":"  Cafe\u0301 🌱  ","color_key":""}`,
		`{"parent_id":"18446744073709551615","name":"  Cafe\u0301 🌱  ","color_key":"green"}`,
	} {
		var request CreateDirectoryRequest
		if err := json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		input, fieldError := ParseCreateDirectoryRequest(request)
		if fieldError != nil || input == nil || input.ParentID != ^uint64(0) || input.Name != "  Cafe\u0301 🌱  " {
			t.Fatal("JSON 解码丢失大整数或 Unicode 原文", fieldError)
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil || len(fields) != 3 || string(fields["parent_id"]) != `"18446744073709551615"` {
			t.Fatal("JSON 字段或字符串 ID 错误", err)
		}
	}
}

func TestParseCreateDirectoryRequestConcurrent(t *testing.T) {
	color := "green"
	request := CreateDirectoryRequest{ParentID: "18446744073709551615", Name: "  学习资料 🌱  ", ColorKey: &color}
	var workers sync.WaitGroup
	for worker := 0; worker < 64; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				input, fieldError := ParseCreateDirectoryRequest(request)
				if fieldError != nil || input == nil || input.ParentID != ^uint64(0) || input.Name != request.Name || input.ColorKey == nil || *input.ColorKey != "green" {
					t.Error("并发请求解析错误", fieldError)
					return
				}
				*input.ColorKey = "sand"
			}
		}()
	}
	workers.Wait()
	if color != "green" {
		t.Fatal("并发解析改变了请求颜色")
	}
}
