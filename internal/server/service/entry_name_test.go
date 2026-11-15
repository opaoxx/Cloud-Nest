package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestValidateEntryNameResults(t *testing.T) {
	for _, testCase := range []struct{ name, input, want string }{
		{name: "single character", input: "a", want: "a"},
		{name: "chinese", input: "课程资料", want: "课程资料"},
		{name: "trim ascii spaces", input: "  学习笔记.md  ", want: "学习笔记.md"},
		{name: "trim unicode whitespace", input: "\u3000\u00a0课程资料\u2009", want: "课程资料"},
		{name: "interior whitespace", input: "课程  资料", want: "课程  资料"},
		{name: "case preserved", input: "My Notes.MD", want: "My Notes.MD"},
		{name: "composed unicode", input: "Café.txt", want: "Café.txt"},
		{name: "decomposed unicode preserved", input: "Cafe\u0301.txt", want: "Cafe\u0301.txt"},
		{name: "emoji and joiners", input: "家庭 👨‍👩‍👧‍👦", want: "家庭 👨‍👩‍👧‍👦"},
		{name: "hidden file", input: ".gitignore", want: ".gitignore"},
		{name: "interior periods", input: "a..b.tar.gz", want: "a..b.tar.gz"},
		{name: "ordinary punctuation", input: "50% + # & ! @ $ = , [1] (草稿)", want: "50% + # & ! @ $ = , [1] (草稿)"},
		{name: "device prefix", input: "CONSOLE.txt", want: "CONSOLE.txt"},
		{name: "device with suffix", input: "NUL-safe.txt", want: "NUL-safe.txt"},
		{name: "zero numbered port", input: "COM0", want: "COM0"},
		{name: "two digit port", input: "COM10.txt", want: "COM10.txt"},
		{name: "zero printer", input: "LPT0", want: "LPT0"},
		{name: "two digit printer", input: "LPT10", want: "LPT10"},
		{name: "other superscript digit", input: "COM⁴", want: "COM⁴"},
		{name: "leading period before device", input: ".CON", want: ".CON"},
		{name: "console name with extension", input: "CONIN$.txt", want: "CONIN$.txt"},
		{name: "console output with extension", input: "CONOUT$.txt", want: "CONOUT$.txt"},
		{name: "ascii upper bound", input: strings.Repeat("a", 120), want: strings.Repeat("a", 120)},
		{name: "bmp upper bound", input: strings.Repeat("中", 120), want: strings.Repeat("中", 120)},
		{name: "emoji upper bound", input: strings.Repeat("🌱", 60), want: strings.Repeat("🌱", 60)},
		{name: "mixed utf16 bound", input: strings.Repeat("a", 118) + "🌱", want: strings.Repeat("a", 118) + "🌱"},
		{name: "trim before length", input: strings.Repeat(" ", 256) + "a" + strings.Repeat(" ", 256), want: "a"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actual, err := ValidateEntryName(testCase.input)
			if err != nil || actual != testCase.want || len(utf16.Encode([]rune(actual))) < 1 || len(utf16.Encode([]rune(actual))) > 120 {
				t.Fatal("有效名称的去空白、原文或 UTF-16 长度错误", err)
			}
			second, err := ValidateEntryName(actual)
			if err != nil || second != actual {
				t.Fatal("有效名称重复校验未保持相同结果", err)
			}
		})
	}
}

func TestValidateEntryNameFailures(t *testing.T) {
	for _, testCase := range []struct{ name, input string }{
		{name: "empty"},
		{name: "blank", input: "   "},
		{name: "unicode blank", input: "\u3000\u00a0\u2009"},
		{name: "current directory", input: "."},
		{name: "parent directory", input: ".."},
		{name: "trailing period", input: "课程资料."},
		{name: "period before trailing whitespace", input: "课程资料.\u3000"},
		{name: "ascii overflow", input: strings.Repeat("a", 121)},
		{name: "bmp overflow", input: strings.Repeat("中", 121)},
		{name: "emoji overflow", input: strings.Repeat("🌱", 61)},
		{name: "mixed utf16 overflow", input: strings.Repeat("a", 119) + "🌱"},
		{name: "joined emoji overflow", input: strings.Repeat("👨‍👩‍👧‍👦", 11)},
		{name: "long name", input: strings.Repeat("a", 4096)},
		{name: "leading tab rejected before trim", input: "\t课程资料"},
		{name: "trailing newline rejected before trim", input: "课程资料\n"},
		{name: "private input", input: "private-invalid/name"},
		{name: "invalid utf8", input: "资料\xff"},
		{name: "overlong utf8", input: "\xc0\xaf"},
		{name: "surrogate utf8", input: "\xed\xa0\x80"},
		{name: "incomplete utf8", input: "\xf0\x9f\x8c"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actual, err := ValidateEntryName(testCase.input)
			if actual != "" || !errors.Is(err, domain.ErrInvalidEntryName) || err.Error() != "目录项名称不符合命名规则" {
				t.Fatal("非法名称未返回空结果及固定领域错误", err)
			}
		})
	}
	for _, character := range `<>:"/\|?*` {
		t.Run(fmt.Sprintf("reserved character U+%04X", character), func(t *testing.T) {
			if actual, err := ValidateEntryName("资料" + string(character) + "一"); actual != "" || !errors.Is(err, domain.ErrInvalidEntryName) {
				t.Fatal("路径保留字符未拒绝", err)
			}
		})
	}
	for character := rune(0); character <= 159; character++ {
		if character > 31 && character < 127 {
			continue
		}
		t.Run(fmt.Sprintf("control U+%04X", character), func(t *testing.T) {
			if actual, err := ValidateEntryName("资料" + string(character) + "一"); actual != "" || !errors.Is(err, domain.ErrInvalidEntryName) {
				t.Fatal("控制字符未拒绝", err)
			}
		})
	}
}

func TestValidateEntryNameReservedDevices(t *testing.T) {
	devices := []string{"CON", "PRN", "AUX", "NUL"}
	for _, prefix := range []string{"COM", "LPT"} {
		for _, digit := range "123456789¹²³" {
			devices = append(devices, prefix+string(digit))
		}
	}
	for _, device := range devices {
		for _, name := range []string{device, strings.ToLower(device), device + ".txt", device + ".tar.gz", device + " .txt", "  " + device + "  "} {
			t.Run(name, func(t *testing.T) {
				if actual, err := ValidateEntryName(name); actual != "" || !errors.Is(err, domain.ErrInvalidEntryName) {
					t.Fatal("设备名或带扩展名的设备名未拒绝", err)
				}
			})
		}
	}
	for _, name := range []string{"CONIN$", "CONOUT$", "conin$", "conout$", "  CONIN$  ", "  CONOUT$  "} {
		t.Run(name, func(t *testing.T) {
			if actual, err := ValidateEntryName(name); actual != "" || !errors.Is(err, domain.ErrInvalidEntryName) {
				t.Fatal("控制台设备名未拒绝", err)
			}
		})
	}
}

func TestValidateEntryNameConcurrent(t *testing.T) {
	var workers sync.WaitGroup
	for worker := 0; worker < 64; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				actual, err := ValidateEntryName("  课程资料 🌱  ")
				if err != nil || actual != "课程资料 🌱" {
					t.Error("并发有效名称校验错误", err)
				}
				if actual, err := ValidateEntryName("com¹.txt"); actual != "" || !errors.Is(err, domain.ErrInvalidEntryName) {
					t.Error("并发设备名校验错误", err)
				}
			}
		}()
	}
	workers.Wait()
}

func TestValidateEntryNameSharedExamples(t *testing.T) {
	// 1. 读取 API 目录中的跨客户端共享样例。
	content, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "testdata", "entry_name_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var examples []struct {
		Case        string `json:"case"`
		Input       string `json:"input"`
		Valid       bool   `json:"valid"`
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(content, &examples); err != nil || len(examples) != 24 {
		t.Fatal("共享名称样例读取错误", err)
	}

	// 2. 逐项核对有效标记、实际展示名及统一错误。
	seen := make(map[string]bool, len(examples))
	for _, example := range examples {
		if example.Case == "" || seen[example.Case] {
			t.Fatal("共享名称样例标识缺失或重复")
		}
		seen[example.Case] = true
		t.Run(example.Case, func(t *testing.T) {
			actual, err := ValidateEntryName(example.Input)
			if actual != example.DisplayName || (err == nil) != example.Valid || (!example.Valid && !errors.Is(err, domain.ErrInvalidEntryName)) {
				t.Fatal("名称行为与共享样例不一致", err)
			}
		})
	}
}
