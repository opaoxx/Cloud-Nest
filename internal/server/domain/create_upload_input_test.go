package domain_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestCreateUploadInputIsValid(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		input domain.CreateUploadInput
		valid bool
	}{
		{"ordinary file", domain.CreateUploadInput{ParentID: 12, Name: "学习笔记.md", SizeBytes: 2048}, true},
		{"empty file", domain.CreateUploadInput{ParentID: 12, Name: "empty.bin"}, true},
		{"one byte", domain.CreateUploadInput{ParentID: 12, Name: "a.bin", SizeBytes: 1}, true},
		{"below single limit", domain.CreateUploadInput{ParentID: 12, Name: "a.bin", SizeBytes: 16777215}, true},
		{"single limit", domain.CreateUploadInput{ParentID: 12, Name: "a.bin", SizeBytes: 16777216}, true},
		{"above single limit", domain.CreateUploadInput{ParentID: 12, Name: "a.bin", SizeBytes: 16777217}, false},
		{"maximum unsigned size", domain.CreateUploadInput{ParentID: 12, Name: "a.bin", SizeBytes: ^uint64(0)}, false},
		{"high signed size", domain.CreateUploadInput{ParentID: 12, Name: "a.bin", SizeBytes: 1 << 63}, false},
		{"JSON safe maximum size", domain.CreateUploadInput{ParentID: 12, Name: "a.bin", SizeBytes: 1<<53 - 1}, false},
		{"zero parent", domain.CreateUploadInput{Name: "a.bin", SizeBytes: 1}, false},
		{"empty name", domain.CreateUploadInput{ParentID: 12, SizeBytes: 1}, false},
		{"invalid UTF-8", domain.CreateUploadInput{ParentID: 12, Name: string([]byte{0xff}), SizeBytes: 1}, false},
		{"truncated UTF-8", domain.CreateUploadInput{ParentID: 12, Name: string([]byte{0xe5, 0xad}), SizeBytes: 1}, false},
		{"maximum parent", domain.CreateUploadInput{ParentID: ^uint64(0), Name: "a.bin", SizeBytes: 1}, true},
		{"high unsigned parent", domain.CreateUploadInput{ParentID: 1 << 63, Name: "a.bin", SizeBytes: 1}, true},
		{"emoji name", domain.CreateUploadInput{ParentID: 12, Name: "实验 🌱.txt", SizeBytes: 1}, true},
		{"original name", domain.CreateUploadInput{ParentID: 12, Name: "  Café.PDF  ", SizeBytes: 1}, true},
		{"zero value", domain.CreateUploadInput{}, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 检查基本声明，保留上游提供的完整标识及名称。
			original := fixture.input
			if fixture.input.IsValid() != fixture.valid || fixture.input != original {
				t.Fatal("上传输入的基本校验或原文保护错误")
			}
		})
	}
	if domain.MaxSingleUploadSizeBytes != 16777216 {
		t.Fatal("首期上传上限与已批准的 16 MiB 不符")
	}
}

func TestCreateUploadInputSizeAndParentBounds(t *testing.T) {
	// 1. 使用独立的数值边界，检查零字节及大小上限的包含关系。
	for _, parentID := range []uint64{0, 1, 19, 1 << 63, ^uint64(0)} {
		for _, size := range []uint64{0, 1, 16777215, 16777216, 16777217, 1<<53 - 1, 1 << 63, ^uint64(0)} {
			t.Run(fmt.Sprintf("parent %d size %d", parentID, size), func(t *testing.T) {
				input := domain.CreateUploadInput{ParentID: parentID, Name: "bounds.bin", SizeBytes: size}
				expected := parentID != 0 && size <= 16777216
				if input.IsValid() != expected {
					t.Fatal("完整父目录 ID 或声明大小边界错误")
				}
			})
		}
	}
}

func TestCreateUploadInputOptionalHashAndPreservation(t *testing.T) {
	for _, mode := range []string{"omitted", "all zero digest", "empty file digest", "content digest", "binary digest", "invalid size"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 用 nil 区分未声明哈希，非空数组明确承接 BINARY(32)。
			input := domain.CreateUploadInput{ParentID: 12, Name: "  Upload.MD  ", SizeBytes: 2048}
			var digest [sha256.Size]byte
			switch mode {
			case "empty file digest":
				input.SizeBytes = 0
				digest = sha256.Sum256(nil)
			case "content digest":
				content := []byte("CloudNest")
				input.SizeBytes = uint64(len(content))
				digest = sha256.Sum256(content)
			case "binary digest":
				for index := range digest {
					digest[index] = byte(index * 7)
				}
			case "invalid size":
				input.SizeBytes = 16777217
				digest = sha256.Sum256([]byte("declaration"))
			}
			if mode != "omitted" {
				input.ExpectedSHA256 = &digest
			}
			original, originalDigest := input, digest
			// 2. 重复校验只读取声明，保留名称、哈希指针及全部字节。
			for range 3 {
				if input.IsValid() != (mode != "invalid size") || input != original || digest != originalDigest {
					t.Fatal("上传声明校验修改了来源或错误处理大小")
				}
			}
			if mode == "omitted" {
				if input.ExpectedSHA256 != nil {
					t.Fatal("未声明哈希被赋予摘要")
				}
				return
			}
			if input.ExpectedSHA256 == nil || len(*input.ExpectedSHA256) != 32 || *input.ExpectedSHA256 != originalDigest {
				t.Fatal("上传声明未保留完整预期哈希")
			}
		})
	}
}

func TestCreateUploadInputNamingCompatibility(t *testing.T) {
	for _, fixture := range []struct{ name, raw, display string }{
		{"trimmed name", "  学习笔记.md  ", "学习笔记.md"},
		{"Unicode form", "Café.MD", "Café.MD"},
		{"ASCII limit", strings.Repeat("a", 120), strings.Repeat("a", 120)},
		{"emoji limit", strings.Repeat("🌱", 60), strings.Repeat("🌱", 60)},
		{"blank name", "   ", ""},
		{"long name", strings.Repeat("a", 121), ""},
		{"reserved name", "CON.txt", ""},
		{"path separator", "a/b.txt", ""},
		{"control character", "a\x00.bin", ""},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 输入模型接收原始 UTF-8 声明，命名服务实施完整命名规则。
			input := domain.CreateUploadInput{ParentID: 12, Name: fixture.raw, SizeBytes: 1}
			if fixture.name == "control character" && !strings.ContainsRune(input.Name, 0) {
				t.Fatal("控制字符夹具须包含实际 NUL")
			}
			if !input.IsValid() {
				t.Fatal("名称服务衔接前的基本声明无效")
			}
			display, err := service.ValidateEntryName(input.Name)
			if fixture.display == "" {
				if display != "" || !errors.Is(err, domain.ErrInvalidEntryName) {
					t.Fatal("统一命名服务接受了无效上传名称", err)
				}
				return
			}
			if err != nil || display != fixture.display {
				t.Fatal("上传名称与已有命名规则不一致", err)
			}
			// 2. 展示名称继续使用已有名称键生成规则，原始输入独立保留。
			key, err := service.BuildEntryNameKey(display)
			if err != nil || len(key) == 0 || input.Name != fixture.raw {
				t.Fatal("上传名称键衔接或原文保护错误", err)
			}
		})
	}
}
