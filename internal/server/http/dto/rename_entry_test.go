package dto

import (
	"strings"
	"sync"
	"testing"
)

func TestParseRenameEntryRequestResults(t *testing.T) {
	for _, version := range []struct {
		text string
		want uint64
	}{
		{"1", 1}, {"7", 7}, {"9007199254740993", 9007199254740993},
		{"9223372036854775808", 9223372036854775808}, {"18446744073709551615", ^uint64(0)},
	} {
		for _, name := range []string{
			"学习资料", "  My Notes 🌱  ", "Cafe\u0301", "   ", "目录/一", "CON.txt",
			strings.Repeat(" ", 256) + "a", strings.Repeat("a", 121), "a\x00b",
		} {
			request := RenameEntryRequest{Name: name, ExpectedRevision: version.text}
			input, fieldError := ParseRenameEntryRequest(request)
			if fieldError != nil || input == nil || input.Name != name || input.ExpectedRevision != version.want {
				t.Fatal("名称原文或完整无符号版本未准确解析", fieldError)
			}
			if request.Name != name || request.ExpectedRevision != version.text {
				t.Fatal("解析修改了原始请求")
			}
		}
	}
}

func TestParseRenameEntryRequestFailures(t *testing.T) {
	for _, name := range []string{"", "private-name\xff", "\xc0\xaf", "\xed\xa0\x80", "\xf4\x90\x80\x80"} {
		for _, version := range []string{"1", "private-version"} {
			input, fieldError := ParseRenameEntryRequest(RenameEntryRequest{Name: name, ExpectedRevision: version})
			if input != nil || fieldError == nil || fieldError.Field != "name" || fieldError.Message != "名称须为非空的有效 UTF-8 文本" {
				t.Fatal("名称错误或名称优先顺序错误", fieldError)
			}
		}
	}
	for _, version := range []string{
		"", "0", "00", "01", "+1", "-1", " 1", "1 ", "1\n", "1\t", "1\r\n",
		"1.0", "1e1", "0x10", "１", "١", "18446744073709551616", strings.Repeat("9", 100), "private-version", "\xff",
	} {
		input, fieldError := ParseRenameEntryRequest(RenameEntryRequest{Name: "资料", ExpectedRevision: version})
		if input != nil || fieldError == nil || fieldError.Field != "expected_revision" || fieldError.Message != "预期版本须为 1～18446744073709551615 的标准十进制整数" {
			t.Fatal("非法版本未返回固定字段错误", fieldError)
		}
	}
}

func TestParseRenameEntryRequestIndependent(t *testing.T) {
	request := RenameEntryRequest{Name: "  Cafe\u0301 🌱  ", ExpectedRevision: "18446744073709551615"}
	first, firstError := ParseRenameEntryRequest(request)
	second, secondError := ParseRenameEntryRequest(request)
	if firstError != nil || secondError != nil || first == second {
		t.Fatal("请求解析未返回独立结果")
	}
	first.Name, first.ExpectedRevision = "修改本地输入", 1
	if second.Name != request.Name || second.ExpectedRevision != ^uint64(0) {
		t.Fatal("修改结果影响其他请求")
	}
	_, errorOne := ParseRenameEntryRequest(RenameEntryRequest{})
	_, errorTwo := ParseRenameEntryRequest(RenameEntryRequest{})
	if errorOne == nil || errorTwo == nil || errorOne == errorTwo {
		t.Fatal("字段错误共享可变状态")
	}
	errorOne.Message = "本地修改"
	if errorTwo.Message != "名称须为非空的有效 UTF-8 文本" {
		t.Fatal("修改一次错误影响其他请求")
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				input, fieldError := ParseRenameEntryRequest(request)
				if fieldError != nil || input.Name != request.Name || input.ExpectedRevision != ^uint64(0) {
					t.Error("并发解析结果错误", fieldError)
					return
				}
			}
		}()
	}
	workers.Wait()
}
