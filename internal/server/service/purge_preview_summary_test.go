package service

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSummarizePurgePreviewSubtree(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		entries []domain.PurgePreviewSubtreeEntry
		want    domain.PurgePreviewSummary
	}{
		{
			name: "mixed states and logical files",
			entries: []domain.PurgePreviewSubtreeEntry{
				{ID: 17, EntryType: "DIR", Status: "PURGING", Revision: 1},
				{ID: 9, EntryType: "FILE", Status: "TRASHED", Revision: 2, FileSize: 2048},
				{ID: 14, EntryType: "DIR", Status: "TRASHED", Revision: 4},
				{ID: 15, EntryType: "FILE", Status: "ACTIVE", Revision: 5, FileSize: 2048},
				{ID: 16, EntryType: "FILE", Status: "PURGING", Revision: 3, FileSize: 512},
				{ID: 8, EntryType: "FILE", Status: "PURGED", Revision: 7, FileSize: math.MaxUint64},
				{ID: 20, EntryType: "DIR", Status: "PURGED", Revision: 2},
			},
			want: domain.PurgePreviewSummary{FileCount: 3, DirectoryCount: 2, LogicalBytes: 4608},
		},
		{
			name:    "empty directory",
			entries: []domain.PurgePreviewSubtreeEntry{{ID: 1, EntryType: "DIR", Status: "TRASHED", Revision: 1}},
			want:    domain.PurgePreviewSummary{DirectoryCount: 1},
		},
		{
			name:    "zero-byte file",
			entries: []domain.PurgePreviewSubtreeEntry{{ID: 1, EntryType: "FILE", Status: "TRASHED", Revision: 1}},
			want:    domain.PurgePreviewSummary{FileCount: 1},
		},
		{
			name: "exact maximum uint64 total",
			entries: []domain.PurgePreviewSubtreeEntry{
				{ID: 1, EntryType: "DIR", Status: "TRASHED", Revision: 1},
				{ID: 2, EntryType: "FILE", Status: "ACTIVE", Revision: 1, FileSize: math.MaxUint64 - 1},
				{ID: 3, EntryType: "FILE", Status: "ACTIVE", Revision: 1, FileSize: 1},
			},
			want: domain.PurgePreviewSummary{FileCount: 2, DirectoryCount: 1, LogicalBytes: math.MaxUint64},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			before := slices.Clone(scenario.entries)
			actual, err := SummarizePurgePreviewSubtree(scenario.entries)
			if err != nil || actual != scenario.want || !slices.Equal(before, scenario.entries) {
				t.Fatalf("实际汇总 %+v，期望 %+v，错误 %v", actual, scenario.want, err)
			}
		})
	}
}

func TestSummarizePurgePreviewSubtreeRejectsInvalidData(t *testing.T) {
	root := domain.PurgePreviewSubtreeEntry{ID: 1, EntryType: "DIR", Status: "TRASHED", Revision: 1}
	for _, scenario := range []struct {
		name  string
		entry domain.PurgePreviewSubtreeEntry
	}{
		{"duplicate ID", root},
		{"zero ID", domain.PurgePreviewSubtreeEntry{EntryType: "FILE", Status: "ACTIVE", Revision: 1}},
		{"zero revision", domain.PurgePreviewSubtreeEntry{ID: 2, EntryType: "FILE", Status: "ACTIVE"}},
		{"unknown type", domain.PurgePreviewSubtreeEntry{ID: 2, EntryType: "OTHER", Status: "ACTIVE", Revision: 1}},
		{"unknown status", domain.PurgePreviewSubtreeEntry{ID: 2, EntryType: "FILE", Status: "OTHER", Revision: 1}},
		{"purged directory with size", domain.PurgePreviewSubtreeEntry{ID: 2, EntryType: "DIR", Status: "PURGED", Revision: 1, FileSize: 1}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if result, err := SummarizePurgePreviewSubtree([]domain.PurgePreviewSubtreeEntry{root, scenario.entry}); result != (domain.PurgePreviewSummary{}) ||
				!errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常记录未被拒绝或仍交付部分统计", result, err)
			}
		})
	}

	t.Run("empty tree", func(t *testing.T) {
		if result, err := SummarizePurgePreviewSubtree(nil); result != (domain.PurgePreviewSummary{}) || !errors.Is(err, domain.ErrInvalidFileEntryData) {
			t.Fatal("空子树未返回数据异常", result, err)
		}
	})

	t.Run("only tombstones", func(t *testing.T) {
		entry := root
		entry.Status = "PURGED"
		if result, err := SummarizePurgePreviewSubtree([]domain.PurgePreviewSubtreeEntry{entry}); result != (domain.PurgePreviewSummary{}) ||
			!errors.Is(err, domain.ErrInvalidFileEntryData) {
			t.Fatal("只剩墓碑的子树未被拒绝", result, err)
		}
	})

	t.Run("one byte above uint64 maximum", func(t *testing.T) {
		entries := []domain.PurgePreviewSubtreeEntry{
			root,
			{ID: 2, EntryType: "FILE", Status: "ACTIVE", Revision: 2, FileSize: math.MaxUint64},
			{ID: 3, EntryType: "FILE", Status: "TRASHED", Revision: 3, FileSize: 1},
		}
		if result, err := SummarizePurgePreviewSubtree(entries); result != (domain.PurgePreviewSummary{}) || !errors.Is(err, domain.ErrInvalidFileEntryData) {
			t.Fatal("字节溢出未被拒绝或仍交付部分统计", result, err)
		}
	})
}

func TestSummarizePurgePreviewSubtreeScopeLimit(t *testing.T) {
	entries := make([]domain.PurgePreviewSubtreeEntry, domain.MaxFileEntrySubtreeNodes)
	for index := range entries {
		entries[index] = domain.PurgePreviewSubtreeEntry{ID: uint64(index + 1), EntryType: "DIR", Status: "PURGED", Revision: 1}
	}
	entries[0].Status = "TRASHED"
	result, err := SummarizePurgePreviewSubtree(entries)
	if err != nil || result != (domain.PurgePreviewSummary{DirectoryCount: 1}) {
		t.Fatal("恰好 10000 条保留记录的汇总不符", result, err)
	}
	entries = append(entries, domain.PurgePreviewSubtreeEntry{ID: uint64(len(entries) + 1), EntryType: "DIR", Status: "PURGED", Revision: 1})
	if result, err := SummarizePurgePreviewSubtree(entries); result != (domain.PurgePreviewSummary{}) || !errors.Is(err, domain.ErrFileEntrySubtreeTooLarge) {
		t.Fatal("10001 条保留记录未整体拒绝", result, err)
	}
}
