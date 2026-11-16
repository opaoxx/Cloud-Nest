package dto

import (
	"strings"
	"sync"
	"testing"
)

func TestParseMoveEntryRequestResults(t *testing.T) {
	values := []struct {
		text  string
		value uint64
	}{
		{"1", 1}, {"7", 7}, {"9007199254740993", 9007199254740993}, {"9223372036854775808", 9223372036854775808}, {"18446744073709551615", ^uint64(0)},
	}
	for _, parent := range values {
		for _, version := range values {
			request := MoveEntryRequest{TargetParentID: parent.text, ExpectedRevision: version.text}
			input, fieldError := ParseMoveEntryRequest(request)
			if fieldError != nil || input == nil || input.TargetParentID != parent.value || input.ExpectedRevision != version.value {
				t.Fatal("移动参数未保留完整无符号整数", fieldError)
			}
			if request.TargetParentID != parent.text || request.ExpectedRevision != version.text {
				t.Fatal("解析修改了请求")
			}
		}
	}
}

func TestParseMoveEntryRequestFailures(t *testing.T) {
	invalidValues := []string{"", "0", "00", "01", "+1", "-1", " 1", "1 ", "1\n", "1\t", "1\r\n", "1.0", "1e1", "0x10", "１", "١", "18446744073709551616", strings.Repeat("9", 100), "private-value", "\xff"}
	for _, raw := range invalidValues {
		for _, version := range []string{"1", "private-version"} {
			input, fieldError := ParseMoveEntryRequest(MoveEntryRequest{TargetParentID: raw, ExpectedRevision: version})
			if input != nil || fieldError == nil || fieldError.Field != "target_parent_id" || fieldError.Message != "目标父目录 ID 须为 1～18446744073709551615 的标准十进制整数" {
				t.Fatal("父目录错误或校验顺序错误", fieldError)
			}
		}
		input, fieldError := ParseMoveEntryRequest(MoveEntryRequest{TargetParentID: "12", ExpectedRevision: raw})
		if input != nil || fieldError == nil || fieldError.Field != "expected_revision" || fieldError.Message != "预期版本须为 1～18446744073709551615 的标准十进制整数" {
			t.Fatal("版本错误定位错误", fieldError)
		}
	}
}

func TestParseMoveEntryRequestIndependent(t *testing.T) {
	request := MoveEntryRequest{TargetParentID: "18446744073709551615", ExpectedRevision: "9007199254740993"}
	first, firstError := ParseMoveEntryRequest(request)
	second, secondError := ParseMoveEntryRequest(request)
	if firstError != nil || secondError != nil || first == second {
		t.Fatal("移动解析结果未独立持有")
	}
	first.TargetParentID, first.ExpectedRevision = 1, 1
	if second.TargetParentID != ^uint64(0) || second.ExpectedRevision != 9007199254740993 {
		t.Fatal("修改结果影响其他请求")
	}
	_, errorOne := ParseMoveEntryRequest(MoveEntryRequest{})
	_, errorTwo := ParseMoveEntryRequest(MoveEntryRequest{})
	if errorOne == nil || errorTwo == nil || errorOne == errorTwo {
		t.Fatal("错误共享可变状态")
	}
	errorOne.Message = "本地修改"
	if errorTwo.Message != "目标父目录 ID 须为 1～18446744073709551615 的标准十进制整数" {
		t.Fatal("修改错误影响其他请求")
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				input, fieldError := ParseMoveEntryRequest(request)
				if fieldError != nil || input.TargetParentID != ^uint64(0) || input.ExpectedRevision != 9007199254740993 {
					t.Error("并发移动解析结果错误", fieldError)
					return
				}
			}
		}()
	}
	workers.Wait()
}
