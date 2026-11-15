package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestBuildEntryNameKeyResults(t *testing.T) {
	for _, testCase := range []struct{ name, input, want string }{
		{name: "chinese", input: "课程资料", want: "课程资料"},
		{name: "trim and fold", input: "\u3000 My Notes.MD \u00a0", want: "my notes.md"},
		{name: "composed", input: "Café.txt", want: "café.txt"},
		{name: "decomposed", input: "Cafe\u0301.txt", want: "café.txt"},
		{name: "full fold expansion", input: "Straße.MD", want: "strasse.md"},
		{name: "capital sharp s", input: "STRAẞE", want: "strasse"},
		{name: "ligature expansion", input: "oﬃce", want: "office"},
		{name: "sigma forms", input: "Σσς", want: "σσσ"},
		{name: "default dotted i", input: "İ", want: "i\u0307"},
		{name: "dotless i", input: "ı", want: "ı"},
		{name: "ascii i", input: "I", want: "i"},
		{name: "hangul composition", input: "\u1100\u1161\u11a8", want: "각"},
		{name: "canonical singleton", input: "K", want: "k"},
		{name: "normalize after fold", input: "ǰ", want: "ǰ"},
		{name: "normalize before fold", input: "α\u0345\u0300", want: "ὰι"},
		{name: "stable equal classes", input: "q\u0301\u0300", want: "q\u0301\u0300"},
		{name: "composition blocked", input: "A\u0305\u030a", want: "a\u0305\u030a"},
		{name: "composition after lower class", input: "A\u0327\u030a", want: "å\u0327"},
		{name: "leading nonstarters", input: "\u0301\u0323Q", want: "\u0323\u0301q"},
		{name: "fullwidth preserved", input: "Ａ", want: "ａ"},
		{name: "circled preserved", input: "①", want: "①"},
		{name: "internal whitespace preserved", input: "A\u00a0 B", want: "a\u00a0 b"},
		{name: "emoji joiners preserved", input: "家庭 👨‍👩‍👧‍👦", want: "家庭 👨‍👩‍👧‍👦"},
		{name: "ascii upper bound", input: strings.Repeat("A", 120), want: strings.Repeat("a", 120)},
		{name: "bmp upper bound", input: strings.Repeat("中", 120), want: strings.Repeat("中", 120)},
		{name: "emoji upper bound", input: strings.Repeat("🌱", 60), want: strings.Repeat("🌱", 60)},
		{name: "tibetan byte expansion", input: strings.Repeat("\u0f73", 120), want: strings.Repeat("\u0f71", 120) + strings.Repeat("\u0f72", 120)},
		{name: "hebrew byte expansion", input: strings.Repeat("\ufb2c", 120), want: strings.Repeat("ש\u05bc\u05c1", 120)},
		{name: "musical byte expansion", input: strings.Repeat("\U0001d160", 60), want: strings.Repeat("\U0001d158\U0001d165\U0001d16e", 60)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actual, err := BuildEntryNameKey(testCase.input)
			if err != nil || actual == nil || string(actual) != testCase.want || !utf8.Valid(actual) || len(actual) > maxEntryNameKeyBytes {
				t.Fatal("名称键、UTF-8 编码或字节长度错误", err)
			}
			if strings.HasSuffix(testCase.name, "byte expansion") && len(actual) != 720 {
				t.Fatal("规范分解后的 720 字节名称键错误")
			}
		})
	}
}

func TestBuildEntryNameKeyLongCombiningSequences(t *testing.T) {
	for _, count := range []int{29, 30, 31, 60, 118} {
		// 1. 两种顺序表示同一规范等价文本，名称都满足 120 个 UTF-16 单元上限。
		first := "q" + strings.Repeat("\u0301", count) + "\u0323"
		second := "q\u0323" + strings.Repeat("\u0301", count)
		firstKey, firstErr := BuildEntryNameKey(first)
		secondKey, secondErr := BuildEntryNameKey(second)
		if firstErr != nil || secondErr != nil || string(firstKey) != second || !bytes.Equal(firstKey, secondKey) || bytes.Contains(firstKey, []byte("\u034f")) {
			t.Fatalf("%d 个组合附加符的规范等价键错误", count)
		}

		// 2. 原始 CGJ 有语义，保留它对重排的阻挡效果。
		withJoiner := "q" + strings.Repeat("\u0301", count-1) + "\u034f\u0323"
		joinerKey, err := BuildEntryNameKey(withJoiner)
		if err != nil || string(joinerKey) != withJoiner || bytes.Equal(firstKey, joinerKey) {
			t.Fatal("名称中原有的组合字素连接符未保留", err)
		}
	}
}

func TestBuildEntryNameKeyDistinctNames(t *testing.T) {
	for _, names := range [][2]string{
		{"café", "cafe"},
		{"Ａ", "A"},
		{"①", "1"},
		{"A", "А"},
		{"İ", "i"},
		{"ı", "I"},
		{"a b", "a  b"},
		{"a b", "a\u00a0b"},
		{"q\u0301\u0300", "q\u0300\u0301"},
		{"👨‍👩‍👧‍👦", "👨👩👧👦"},
		{"❤", "❤️"},
	} {
		first, firstErr := BuildEntryNameKey(names[0])
		second, secondErr := BuildEntryNameKey(names[1])
		if firstErr != nil || secondErr != nil || bytes.Equal(first, second) {
			t.Fatal("不同名称的判重键发生错误合并", firstErr, secondErr)
		}
	}
}

func TestBuildEntryNameKeyFailures(t *testing.T) {
	for _, name := range []string{
		"", " ", ".", "..", "资料.", "资料/一", "资料\\一", "资料:一", "\t资料", "资料\n", "资料\x00", "资料\xff",
		"con.txt", "COM¹.txt", "CONIN$", strings.Repeat("A", 121), strings.Repeat("🌱", 61),
	} {
		actual, err := BuildEntryNameKey(name)
		if actual != nil || !errors.Is(err, domain.ErrInvalidEntryName) || err.Error() != "目录项名称不符合命名规则" {
			t.Fatal("非法名称未返回 nil 和固定名称领域错误", err)
		}
	}
}

func TestBuildEntryNameKeyOwnedResults(t *testing.T) {
	input := "  Straße.MD  "
	first, firstErr := BuildEntryNameKey(input)
	second, secondErr := BuildEntryNameKey(input)
	if firstErr != nil || secondErr != nil || !bytes.Equal(first, second) {
		t.Fatal("重复计算名称键结果不一致")
	}
	first[0] = '!'
	third, err := BuildEntryNameKey(input)
	if err != nil || string(second) != "strasse.md" || string(third) != "strasse.md" || input != "  Straße.MD  " {
		t.Fatal("名称键输出共享了可变数据", err)
	}
}

func TestBuildEntryNameKeyConcurrent(t *testing.T) {
	var workers sync.WaitGroup
	for worker := 0; worker < 64; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				actual, err := BuildEntryNameKey("  STRAẞE Cafe\u0301  ")
				if err != nil || string(actual) != "strasse café" {
					t.Error("并发名称键计算错误", err)
				}
				if actual, err := BuildEntryNameKey("con.txt"); actual != nil || !errors.Is(err, domain.ErrInvalidEntryName) {
					t.Error("并发非法名称未拒绝", err)
				}
			}
		}()
	}
	workers.Wait()
}

func TestBuildEntryNameKeySharedExamples(t *testing.T) {
	// 1. 读取客户端与服务端共用的名称、有效标记、展示名和名称键文本。
	content, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "testdata", "entry_name_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var examples []struct {
		Case        string `json:"case"`
		Input       string `json:"input"`
		Valid       bool   `json:"valid"`
		DisplayName string `json:"display_name"`
		NameKey     string `json:"name_key"`
	}
	if err := json.Unmarshal(content, &examples); err != nil || len(examples) != 40 {
		t.Fatal("共享名称键样例读取错误", err)
	}

	// 2. 核对实际字节，并验证展示名仍保留用户的大小写及 Unicode 写法。
	seen := make(map[string]bool, len(examples))
	for _, example := range examples {
		if example.Case == "" || seen[example.Case] {
			t.Fatal("共享名称键样例标识缺失或重复")
		}
		seen[example.Case] = true
		t.Run(example.Case, func(t *testing.T) {
			key, err := BuildEntryNameKey(example.Input)
			if (err == nil) != example.Valid || string(key) != example.NameKey || (!example.Valid && (key != nil || !errors.Is(err, domain.ErrInvalidEntryName))) {
				t.Fatal("名称键与共享样例不一致", err)
			}
			if example.Valid {
				displayName, err := ValidateEntryName(example.Input)
				if err != nil || displayName != example.DisplayName {
					t.Fatal("名称键计算影响展示名校验", err)
				}
			}
		})
	}
}
