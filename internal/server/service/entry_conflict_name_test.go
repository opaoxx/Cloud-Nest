package service

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestBuildRestoreConflictName(t *testing.T) {
	for _, scenario := range []struct {
		name, original, entryType string
		number                    int
		want                      string
		wantError                 error
	}{
		{name: "file keeps last extension", original: "归档.tar.gz", entryType: "FILE", number: 1, want: "归档.tar (1).gz"},
		{name: "directory keeps dots in base", original: "归档.tar.gz", entryType: "DIR", number: 1, want: "归档.tar.gz (1)"},
		{name: "file without extension", original: "README", entryType: "FILE", number: 2, want: "README (2)"},
		{name: "single leading dot", original: ".env", entryType: "FILE", number: 1, want: ".env (1)"},
		{name: "dotfile with extension", original: ".config.JSON", entryType: "FILE", number: 3, want: ".config (3).JSON"},
		{name: "previous number is part of directory name", original: "备份 (2)", entryType: "DIR", number: 3, want: "备份 (2) (3)"},
		{name: "file base shortened for a two-digit suffix", original: strings.Repeat("a", 116) + ".PDF", entryType: "FILE", number: 10, want: strings.Repeat("a", 111) + " (10).PDF"},
		{name: "directory base shortened for a two-digit suffix", original: strings.Repeat("a", 116) + ".PDF", entryType: "DIR", number: 10, want: strings.Repeat("a", 115) + " (10)"},
		{name: "file preserves complete supplementary character", original: strings.Repeat("🌱", 58) + ".ab", entryType: "FILE", number: 1, want: strings.Repeat("🌱", 56) + " (1).ab"},
		{name: "directory preserves complete supplementary character", original: strings.Repeat("🌱", 60), entryType: "DIR", number: 10, want: strings.Repeat("🌱", 57) + " (10)"},
		{name: "long file extension leaves no base", original: "a." + strings.Repeat("x", 115), entryType: "FILE", number: 1, wantError: domain.ErrEntryNameConflict},
		{name: "same long name fits as directory", original: "a." + strings.Repeat("x", 115), entryType: "DIR", number: 1, want: "a." + strings.Repeat("x", 114) + " (1)"},
		{name: "unknown entry type", original: "笔记.txt", entryType: "OTHER", number: 1, wantError: domain.ErrInvalidFileEntryData},
		{name: "suffix must be positive", original: "笔记.txt", entryType: "FILE", number: 0, wantError: domain.ErrInvalidEntryName},
		{name: "negative directory suffix", original: "课程资料", entryType: "DIR", number: -1, wantError: domain.ErrInvalidEntryName},
		{name: "path separator is invalid", original: "旧目录/新目录", entryType: "DIR", number: 1, wantError: domain.ErrInvalidEntryName},
		{name: "original directory over length limit", original: strings.Repeat("🌱", 61), entryType: "DIR", number: 1, wantError: domain.ErrInvalidEntryName},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actual, err := BuildRestoreConflictName(scenario.original, scenario.entryType, scenario.number)
			if actual != scenario.want || !errors.Is(err, scenario.wantError) {
				t.Fatalf("候选为 %q，错误为 %v，期望 %q 和 %v", actual, err, scenario.want, scenario.wantError)
			}
			if units := len(utf16.Encode([]rune(actual))); units > 120 {
				t.Fatalf("候选名有 %d 个 UTF-16 代码单元，超过 120", units)
			}
		})
	}
}
