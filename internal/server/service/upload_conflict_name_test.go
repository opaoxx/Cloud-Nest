package service

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestBuildUploadConflictName(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		rawName      string
		suffixNumber int
		want         string
	}{
		{"first candidate", "笔记.txt", 1, "笔记 (1).txt"},
		{"next candidate starts from original name", "笔记.txt", 2, "笔记 (2).txt"},
		{"existing numbered text stays in the base", "笔记 (1).txt", 2, "笔记 (1) (2).txt"},
		{"trim outer spaces and preserve case", "  Report.PDF\u3000", 12, "Report (12).PDF"},
		{"without extension", "README", 1, "README (1)"},
		{"dotfile", ".env", 1, ".env (1)"},
		{"dotfile with extension", ".config.JSON", 3, ".config (3).JSON"},
		{"use final extension", "archive.tar.GZ", 1, "archive.tar (1).GZ"},
		{"consecutive dots", "a..TXT", 1, "a. (1).TXT"},
		{"fullwidth dot", "资料．PDF", 1, "资料．PDF (1)"},
		{"retain decomposed spelling", "Cafe\u0301.txt", 1, "Cafe\u0301 (1).txt"},
		{"base and extension fit just at limit", "a." + strings.Repeat("x", 114), 1, "a (1)." + strings.Repeat("x", 114)},
		{"truncate ASCII base for one digit", strings.Repeat("a", 116) + ".txt", 9, strings.Repeat("a", 112) + " (9).txt"},
		{"truncate one more unit for two digits", strings.Repeat("a", 116) + ".txt", 10, strings.Repeat("a", 111) + " (10).txt"},
		{"odd budget preserves a whole supplementary character", strings.Repeat("🌱", 58) + ".ab", 1, strings.Repeat("🌱", 56) + " (1).ab"},
		{"count supplementary characters in extension", strings.Repeat("a", 115) + ".🌱", 1, strings.Repeat("a", 113) + " (1).🌱"},
		{"full-length name with no extension", strings.Repeat("🌱", 60), 1, strings.Repeat("🌱", 58) + " (1)"},
		{"largest positive suffix number", "a.txt", math.MaxInt, "a (" + strconv.Itoa(math.MaxInt) + ").txt"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actual, err := BuildUploadConflictName(scenario.rawName, scenario.suffixNumber)
			if err != nil || actual != scenario.want {
				t.Fatalf("候选名为 %q，期望 %q，错误 %v", actual, scenario.want, err)
			}
			if units := len(utf16.Encode([]rune(actual))); units > 120 {
				t.Fatalf("候选名长度超限，UTF-16 代码单元 %d", units)
			}
		})
	}
}

func TestBuildUploadConflictNameErrors(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		rawName      string
		suffixNumber int
		wantError    error
	}{
		{"empty original name", "", 1, domain.ErrInvalidEntryName},
		{"invalid UTF-8", "a\xff.txt", 1, domain.ErrInvalidEntryName},
		{"control character", "a\n.txt", 1, domain.ErrInvalidEntryName},
		{"reserved device name", "CON.txt", 1, domain.ErrInvalidEntryName},
		{"path separator", "folder/note.txt", 1, domain.ErrInvalidEntryName},
		{"trailing dot", "note.", 1, domain.ErrInvalidEntryName},
		{"original above limit", strings.Repeat("🌱", 61), 1, domain.ErrInvalidEntryName},
		{"zero suffix number", "note.txt", 0, domain.ErrInvalidEntryName},
		{"negative suffix number", "note.txt", -1, domain.ErrInvalidEntryName},
		{"no room for base character", "a." + strings.Repeat("x", 115), 1, domain.ErrEntryNameConflict},
		{"no room for complete supplementary character", "🌱." + strings.Repeat("x", 114), 1, domain.ErrEntryNameConflict},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actual, err := BuildUploadConflictName(scenario.rawName, scenario.suffixNumber)
			if actual != "" || !errors.Is(err, scenario.wantError) {
				t.Fatalf("错误结果为 %q 和 %v，期望空名称和 %v", actual, err, scenario.wantError)
			}
		})
	}
}
