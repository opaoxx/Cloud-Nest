package service_test

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func uploadDeclarationFixture() domain.CreateUploadInput {
	return domain.CreateUploadInput{ParentID: 12, Name: "file.bin", SizeBytes: 2048}
}

func TestValidateCreateUploadInputResults(t *testing.T) {
	for _, mode := range []string{"ordinary", "zero bytes", "below limit", "at limit", "maximum parent", "high unsigned parent", "trim ASCII", "trim Unicode", "Unicode preserved", "case preserved", "interior spaces", "hidden name", "ASCII limit", "BMP limit", "emoji limit", "trim before length", "zero digest", "empty digest", "binary digest"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备有效声明，区分原始名称与校验后的展示名。
			input := uploadDeclarationFixture()
			display := input.Name
			var digest [sha256.Size]byte
			switch mode {
			case "zero bytes":
				input.SizeBytes = 0
			case "below limit":
				input.SizeBytes = 16777215
			case "at limit":
				input.SizeBytes = 16777216
			case "maximum parent":
				input.ParentID = ^uint64(0)
			case "high unsigned parent":
				input.ParentID = 1 << 63
			case "trim ASCII":
				input.Name, display = "  学习笔记.md  ", "学习笔记.md"
			case "trim Unicode":
				input.Name, display = "\u3000\u00a0课程资料.pdf\u2009", "课程资料.pdf"
			case "Unicode preserved":
				input.Name, display = "Cafe\u0301.MD", "Cafe\u0301.MD"
			case "case preserved":
				input.Name, display = "My Notes.MD", "My Notes.MD"
			case "interior spaces":
				input.Name, display = "课程  资料.md", "课程  资料.md"
			case "hidden name":
				input.Name, display = ".gitignore", ".gitignore"
			case "ASCII limit":
				input.Name = strings.Repeat("a", 120)
				display = input.Name
			case "BMP limit":
				input.Name = strings.Repeat("中", 120)
				display = input.Name
			case "emoji limit":
				input.Name = strings.Repeat("🌱", 60)
				display = input.Name
			case "trim before length":
				input.Name = strings.Repeat(" ", 256) + "a" + strings.Repeat(" ", 256)
				display = "a"
			case "zero digest":
				input.ExpectedSHA256 = &digest
			case "empty digest":
				input.SizeBytes = 0
				digest = sha256.Sum256(nil)
				input.ExpectedSHA256 = &digest
			case "binary digest":
				for index := range digest {
					digest[index] = byte(index * 7)
				}
				input.ExpectedSHA256 = &digest
			}
			original, originalDigest := input, digest
			prepared, err := service.ValidateCreateUploadInput(input)
			if err != nil || prepared == nil || prepared.ParentID != input.ParentID || prepared.Name != display || prepared.SizeBytes != input.SizeBytes || !prepared.IsValid() || (prepared.ExpectedSHA256 == nil) != (input.ExpectedSHA256 == nil) || input != original || digest != originalDigest {
				t.Fatal("规范化上传声明或来源保护错误", err)
			}
			if input.ExpectedSHA256 != nil && (prepared.ExpectedSHA256 == input.ExpectedSHA256 || *prepared.ExpectedSHA256 != originalDigest) {
				t.Fatal("上传预期摘要未独立复制")
			}
			// 2. 再次校验稳定返回相同业务值，并衔接既有名称键规则。
			repeated, err := service.ValidateCreateUploadInput(*prepared)
			if err != nil || !reflect.DeepEqual(repeated, prepared) || repeated == prepared || (prepared.ExpectedSHA256 != nil && repeated.ExpectedSHA256 == prepared.ExpectedSHA256) {
				t.Fatal("重复业务校验或结果独立性错误", err)
			}
			key, err := service.BuildEntryNameKey(prepared.Name)
			if err != nil || len(key) == 0 {
				t.Fatal("上传展示名与名称键规则不一致", err)
			}
		})
	}
}

func TestValidateCreateUploadInputFailures(t *testing.T) {
	for _, name := range []string{"", "   ", "\u3000", "a\x00.bin", "a\n.bin", "a\t.bin", "a\x7f.bin", string([]byte{0xff}), string([]byte{0xe5, 0xad}), "private-name/file.bin", "a\\b.bin", "a:b.bin", "a?b.bin", "name.", ".", "..", "CON.txt", "NUL", "COM1.bin", "LPT9", "CONIN$", "CONOUT$", strings.Repeat("a", 121), strings.Repeat("中", 121), strings.Repeat("🌱", 61)} {
		t.Run("name "+name, func(t *testing.T) {
			input := uploadDeclarationFixture()
			input.Name = name
			original := input
			prepared, err := service.ValidateCreateUploadInput(input)
			if prepared != nil || !errors.Is(err, domain.ErrInvalidEntryName) || input != original || strings.Contains(err.Error(), "private-") {
				t.Fatal("完整名称错误或来源保护不符", err)
			}
		})
	}
	for _, size := range []uint64{16777217, 1<<53 - 1, 1 << 63, ^uint64(0)} {
		input := uploadDeclarationFixture()
		input.SizeBytes = size
		if prepared, err := service.ValidateCreateUploadInput(input); prepared != nil || !errors.Is(err, domain.ErrUploadFileTooLarge) || err.Error() != "文件大小不能超过 16 MiB" {
			t.Fatal("上传大小超限未返回固定领域错误", err)
		}
	}
	input := uploadDeclarationFixture()
	input.ParentID = 0
	if prepared, err := service.ValidateCreateUploadInput(input); prepared != nil || !errors.Is(err, domain.ErrInvalidUploadParentID) || err.Error() != "上传目标目录 ID 须为非零值" {
		t.Fatal("零目录标识未返回固定领域错误", err)
	}
}

func TestValidateCreateUploadInputOrder(t *testing.T) {
	input := domain.CreateUploadInput{Name: "bad/name.bin", SizeBytes: ^uint64(0)}
	// 1. 多项错误同时存在时，固定按目录、名称、大小检查。
	for _, expected := range []error{domain.ErrInvalidUploadParentID, domain.ErrInvalidEntryName, domain.ErrUploadFileTooLarge} {
		if prepared, err := service.ValidateCreateUploadInput(input); prepared != nil || !errors.Is(err, expected) {
			t.Fatal("上传业务错误顺序错误", err)
		}
		switch expected {
		case domain.ErrInvalidUploadParentID:
			input.ParentID = 12
		case domain.ErrInvalidEntryName:
			input.Name = "  Valid.MD  "
		case domain.ErrUploadFileTooLarge:
			input.SizeBytes = 16777216
		}
	}
	prepared, err := service.ValidateCreateUploadInput(input)
	if err != nil || prepared == nil || prepared.Name != "Valid.MD" || prepared.SizeBytes != 16777216 {
		t.Fatal("逐项修正后声明未通过", err)
	}
}

func TestValidateCreateUploadInputCopiesAndConcurrency(t *testing.T) {
	// 1. 两次校验结果及来源分别持有名称、大小和摘要。
	input := uploadDeclarationFixture()
	input.Name = "  Upload.MD  "
	digest := sha256.Sum256([]byte("fixture"))
	input.ExpectedSHA256 = &digest
	first, firstError := service.ValidateCreateUploadInput(input)
	second, secondError := service.ValidateCreateUploadInput(input)
	if firstError != nil || secondError != nil || first == nil || second == nil || first == second || first.ExpectedSHA256 == second.ExpectedSHA256 || first.ExpectedSHA256 == input.ExpectedSHA256 {
		t.Fatal("上传输入结果或摘要共享")
	}
	originalDigest := digest
	first.Name, first.SizeBytes, first.ParentID = "local.bin", 0, 99
	first.ExpectedSHA256[0] ^= 1
	input.Name, input.SizeBytes, input.ParentID = "source.bin", 1, 88
	digest[0] ^= 1
	if second.Name != "Upload.MD" || second.SizeBytes != 2048 || second.ParentID != 12 || *second.ExpectedSHA256 != originalDigest {
		t.Fatal("来源或其他结果影响已确认声明")
	}
	// 2. 只读来源并发校验，各任务可独立修改自己的结果。
	source := uploadDeclarationFixture()
	source.Name = "  Concurrent.MD  "
	source.ExpectedSHA256 = &originalDigest
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() {
			for range 20 {
				prepared, err := service.ValidateCreateUploadInput(source)
				if err != nil || prepared == nil || prepared.Name != "Concurrent.MD" || prepared.SizeBytes != 2048 || prepared.ExpectedSHA256 == nil || *prepared.ExpectedSHA256 != originalDigest {
					t.Error("并发上传声明校验错误", err)
					return
				}
				prepared.Name, prepared.SizeBytes = "local.bin", 0
				prepared.ExpectedSHA256[0] ^= 1
			}
		})
	}
	workers.Wait()
	if source.Name != "  Concurrent.MD  " || source.SizeBytes != 2048 || *source.ExpectedSHA256 != originalDigest {
		t.Fatal("并发校验改变来源声明")
	}
}

func TestValidateCreateUploadInputDTOFlow(t *testing.T) {
	for _, fixture := range []struct {
		name, rawName string
		size          uint64
		expected      error
	}{
		{"ordinary", "  学习笔记.md  ", 2048, nil},
		{"empty file", "empty.bin", 0, nil},
		{"single limit", "limit.bin", 16777216, nil},
		{"above single limit", "private-limit.bin", 16777217, domain.ErrUploadFileTooLarge},
		{"maximum size", "private-limit.bin", ^uint64(0), domain.ErrUploadFileTooLarge},
		{"blank name", "   ", 0, domain.ErrInvalidEntryName},
		{"reserved name", "CON.txt", 0, domain.ErrInvalidEntryName},
		{"path name", "private/path.bin", 0, domain.ErrInvalidEntryName},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 真实 JSON 与 DTO 保留声明，再由业务校验处理命名和能力边界。
			body, err := json.Marshal(map[string]any{"parent_id": "18446744073709551615", "name": fixture.rawName, "size_bytes": fixture.size, "sha256": strings.Repeat("aB", 32)})
			if err != nil {
				t.Fatal(err)
			}
			var request dto.CreateUploadRequest
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatal(err)
			}
			input, fieldError := dto.ParseCreateUploadRequest(request)
			if fieldError != nil || input == nil {
				t.Fatal("上传声明 DTO 衔接错误", fieldError)
			}
			prepared, err := service.ValidateCreateUploadInput(*input)
			if fixture.expected != nil {
				if prepared != nil || !errors.Is(err, fixture.expected) || strings.Contains(err.Error(), "private-") {
					t.Fatal("DTO 后的业务错误类型不符", err)
				}
				return
			}
			if err != nil || prepared == nil || prepared.ParentID != ^uint64(0) || prepared.Name != strings.TrimSpace(fixture.rawName) || prepared.SizeBytes != fixture.size || prepared.ExpectedSHA256 == nil || prepared.ExpectedSHA256[0] != 0xab || prepared.ExpectedSHA256 == input.ExpectedSHA256 || input.Name != fixture.rawName || request.Name != fixture.rawName {
				t.Fatal("上传声明到独立规范化输入的流程错误", err)
			}
		})
	}
}
