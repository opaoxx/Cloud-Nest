package dto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func createUploadRequestFixture() CreateUploadRequest {
	size := uint64(2048)
	return CreateUploadRequest{ParentID: "12", Name: "  学习笔记.md  ", SizeBytes: &size}
}

func TestParseCreateUploadRequestResults(t *testing.T) {
	for _, mode := range []string{
		"ordinary", "zero bytes", "single limit", "above single limit", "maximum size", "high unsigned size", "minimum parent", "maximum parent", "high unsigned parent", "above JSON safe parent",
		"empty file hash", "uppercase hash", "mixed case hash", "zero hash", "raw name", "decomposed name", "blank for naming service", "path for naming service",
	} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备必填声明和可空摘要，保留后续业务检查需要的原值。
			request := createUploadRequestFixture()
			parentID := uint64(12)
			digest := sha256.Sum256(nil)
			hashText := hex.EncodeToString(digest[:])
			switch mode {
			case "zero bytes":
				*request.SizeBytes = 0
			case "single limit":
				*request.SizeBytes = 16777216
			case "above single limit":
				*request.SizeBytes = 16777217
			case "maximum size":
				*request.SizeBytes = ^uint64(0)
			case "high unsigned size":
				*request.SizeBytes = 1 << 63
			case "minimum parent":
				request.ParentID, parentID = "1", 1
			case "maximum parent":
				request.ParentID, parentID = "18446744073709551615", ^uint64(0)
			case "high unsigned parent":
				request.ParentID, parentID = "9223372036854775808", 1<<63
			case "above JSON safe parent":
				request.ParentID, parentID = "9007199254740993", 9007199254740993
			case "empty file hash":
				*request.SizeBytes = 0
				request.SHA256 = &hashText
			case "uppercase hash":
				hashText = strings.ToUpper(hashText)
				request.SHA256 = &hashText
			case "mixed case hash":
				hashText = strings.ToUpper(hashText[:32]) + hashText[32:]
				request.SHA256 = &hashText
			case "zero hash":
				hashText = strings.Repeat("0", 64)
				digest = [sha256.Size]byte{}
				request.SHA256 = &hashText
			case "raw name":
				request.Name = "  Upload 🌱.MD  "
			case "decomposed name":
				request.Name = "Café.md"
			case "blank for naming service":
				request.Name = "   "
			case "path for naming service":
				request.Name = "a/b.md"
			}
			original, size := request, *request.SizeBytes
			originalHash := hashText
			input, fieldError := ParseCreateUploadRequest(request)
			if fieldError != nil || input == nil || input.ParentID != parentID || input.Name != request.Name || input.SizeBytes != size || (input.ExpectedSHA256 == nil) != (request.SHA256 == nil) {
				t.Fatal("上传声明转换、原文或空值错误", fieldError)
			}
			if input.IsValid() != (size <= domain.MaxSingleUploadSizeBytes) || request != original || *request.SizeBytes != size {
				t.Fatal("大小能力边界或请求保护错误")
			}
			if request.SHA256 != nil && (*input.ExpectedSHA256 != digest || *request.SHA256 != originalHash) {
				t.Fatal("完整摘要解码或大小写原文错误")
			}
		})
	}
}

func TestParseCreateUploadRequestFailures(t *testing.T) {
	for _, parent := range []string{"", "0", "00", "01", "+1", "-1", " 1", "1 ", "1.0", "1e1", "0x10", "１", "18446744073709551616", strings.Repeat("9", 100), "private-parent"} {
		t.Run("parent "+parent, func(t *testing.T) {
			request := createUploadRequestFixture()
			request.ParentID = parent
			input, fieldError := ParseCreateUploadRequest(request)
			if input != nil || fieldError == nil || fieldError.Field != "parent_id" || fieldError.Message != "父目录 ID 须为 1～18446744073709551615 的标准十进制整数" {
				t.Fatal("父目录固定字段错误", fieldError)
			}
		})
	}
	for index, name := range []string{"", string([]byte{0xff}), string([]byte{0xe5, 0xad})} {
		t.Run("name "+string(rune('a'+index)), func(t *testing.T) {
			request := createUploadRequestFixture()
			request.Name = name
			input, fieldError := ParseCreateUploadRequest(request)
			if input != nil || fieldError == nil || fieldError.Field != "name" || fieldError.Message != "名称须为非空的有效 UTF-8 文本" {
				t.Fatal("名称固定字段错误", fieldError)
			}
		})
	}
	request := createUploadRequestFixture()
	request.SizeBytes = nil
	if input, fieldError := ParseCreateUploadRequest(request); input != nil || fieldError == nil || fieldError.Field != "size_bytes" || fieldError.Message != "文件大小须为必填的非负整数" {
		t.Fatal("缺失声明大小未拒绝", fieldError)
	}
	for _, hash := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("g", 64), strings.Repeat("a", 63) + " ", " " + strings.Repeat("a", 63), "0x" + strings.Repeat("a", 62), strings.Repeat("a", 63) + "\n", strings.Repeat("a", 63) + "\x00", strings.Repeat("界", 64), strings.Repeat("a", 63) + string([]byte{0xff}), "private-hash"} {
		t.Run("hash "+hash, func(t *testing.T) {
			request := createUploadRequestFixture()
			request.SHA256 = &hash
			input, fieldError := ParseCreateUploadRequest(request)
			if input != nil || fieldError == nil || fieldError.Field != "sha256" || fieldError.Message != "SHA-256 须为 null 或 64 位十六进制文本" || strings.Contains(fieldError.Message, "private-") {
				t.Fatal("摘要固定字段错误", fieldError)
			}
		})
	}
}

func TestParseCreateUploadRequestOrderAndCopies(t *testing.T) {
	// 1. 按父目录、名称、大小、摘要顺序交付固定且独立的字段错误。
	invalidHash := "private-hash"
	request := CreateUploadRequest{SHA256: &invalidHash}
	for _, field := range []string{"parent_id", "name", "size_bytes", "sha256"} {
		first, firstError := ParseCreateUploadRequest(request)
		second, secondError := ParseCreateUploadRequest(request)
		if first != nil || second != nil || firstError == nil || secondError == nil || firstError == secondError || firstError.Field != field || secondError.Field != field {
			t.Fatal("上传字段顺序或错误副本不符")
		}
		firstError.Message = "modified"
		if secondError.Message == "modified" {
			t.Fatal("字段错误共享状态")
		}
		switch field {
		case "parent_id":
			request.ParentID = "12"
		case "name":
			request.Name = "file.bin"
		case "size_bytes":
			size := uint64(0)
			request.SizeBytes = &size
		}
	}
	// 2. 多次转换各自持有固定数组及大小值，来源修改分别保留独立值。
	request = createUploadRequestFixture()
	hash := strings.Repeat("ab", 32)
	request.SHA256 = &hash
	first, firstError := ParseCreateUploadRequest(request)
	second, secondError := ParseCreateUploadRequest(request)
	if firstError != nil || secondError != nil || first == nil || second == nil || first == second || first.ExpectedSHA256 == second.ExpectedSHA256 {
		t.Fatal("上传输入或摘要未独立分配")
	}
	first.ExpectedSHA256[0] = 0
	first.SizeBytes = 0
	hash = strings.Repeat("00", 32)
	*request.SizeBytes = 1
	if second.ExpectedSHA256[0] != 0xab || second.SizeBytes != 2048 || first.ParentID != 12 || second.Name != request.Name {
		t.Fatal("上传声明或摘要存在共享修改")
	}
}

func TestCreateUploadRequestJSON(t *testing.T) {
	for _, suffix := range []string{"", `,"sha256":null`, `,"sha256":"` + strings.Repeat("ab", 32) + `"`} {
		body := `{"parent_id":"18446744073709551615","name":"  Cafe\u0301 🌱  ","size_bytes":0` + suffix + `}`
		var request CreateUploadRequest
		if err := json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		input, fieldError := ParseCreateUploadRequest(request)
		if fieldError != nil || input == nil || input.ParentID != ^uint64(0) || input.SizeBytes != 0 || input.Name != "  Cafe\u0301 🌱  " || (input.ExpectedSHA256 != nil) != (request.SHA256 != nil) {
			t.Fatal("上传 JSON 声明值丢失", fieldError)
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(encoded, &fields) != nil || len(fields) != 4 || string(fields["parent_id"]) != `"18446744073709551615"` || string(fields["size_bytes"]) != "0" {
			t.Fatal("上传 JSON 字段或数值形式错误")
		}
	}
	for _, sizeField := range []string{"", `,"size_bytes":null`} {
		var request CreateUploadRequest
		if err := json.Unmarshal([]byte(`{"parent_id":"12","name":"file.bin"`+sizeField+`}`), &request); err != nil {
			t.Fatal(err)
		}
		if input, fieldError := ParseCreateUploadRequest(request); input != nil || fieldError == nil || fieldError.Field != "size_bytes" {
			t.Fatal("缺失或 null 大小与零字节混淆")
		}
	}
}

func TestParseCreateUploadRequestConcurrent(t *testing.T) {
	request := createUploadRequestFixture()
	hash := strings.Repeat("ab", 32)
	request.SHA256 = &hash
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			for range 20 {
				input, fieldError := ParseCreateUploadRequest(request)
				if fieldError != nil || input == nil || input.SizeBytes != 2048 || input.ExpectedSHA256 == nil || input.ExpectedSHA256[0] != 0xab {
					t.Error("并发上传声明解析错误", fieldError)
					return
				}
				input.ExpectedSHA256[0] = 0
				input.SizeBytes = 0
			}
		})
	}
	workers.Wait()
	if hash != strings.Repeat("ab", 32) || *request.SizeBytes != 2048 {
		t.Fatal("并发解析影响来源声明")
	}
}
