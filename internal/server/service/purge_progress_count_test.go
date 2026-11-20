package service

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestCountPurgeRemainingEntries(t *testing.T) {
	root := domain.PurgePreviewSubtreeEntry{ID: 14, EntryType: "DIR", Status: "PURGING", Revision: 2}
	for _, scenario := range []struct {
		name    string
		entries []domain.PurgePreviewSubtreeEntry
		want    int
	}{
		{
			name: "mixed states with target out of first position",
			entries: []domain.PurgePreviewSubtreeEntry{
				{ID: 3, EntryType: "FILE", Status: "PURGED", Revision: 7, FileSize: 1},
				{ID: 4, EntryType: "DIR", Status: "TRASHED", Revision: 1},
				root,
				{ID: 15, EntryType: "FILE", Status: "ACTIVE", Revision: 1, FileSize: math.MaxUint64},
				{ID: 17, EntryType: "FILE", Status: "PURGING", Revision: 2, FileSize: 1},
				{ID: 18, EntryType: "DIR", Status: "PURGED", Revision: 2},
			},
			want: 4,
		},
		{name: "only target remains", entries: []domain.PurgePreviewSubtreeEntry{
			root, {ID: 15, EntryType: "FILE", Status: "PURGED", Revision: 2, FileSize: 1},
		}, want: 1},
		{name: "single file", entries: []domain.PurgePreviewSubtreeEntry{
			{ID: 14, EntryType: "FILE", Status: "PURGING", Revision: 2},
		}, want: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			before := slices.Clone(scenario.entries)
			count, err := CountPurgeRemainingEntries(root.ID, scenario.entries)
			if err != nil || count != scenario.want || !slices.Equal(scenario.entries, before) {
				t.Fatalf("实际计数 %d，期望 %d，错误 %v", count, scenario.want, err)
			}
		})
	}
}

func TestCountPurgeRemainingEntriesRejectsInvalidScope(t *testing.T) {
	root := domain.PurgePreviewSubtreeEntry{ID: 14, EntryType: "DIR", Status: "PURGING", Revision: 2}
	completed := root
	completed.Status = "PURGED"
	fileRoot := root
	fileRoot.EntryType = "FILE"
	descendant := domain.PurgePreviewSubtreeEntry{ID: 15, EntryType: "FILE", Status: "ACTIVE", Revision: 1}
	invalid := descendant
	invalid.Status = "UNKNOWN"
	for _, scenario := range []struct {
		name    string
		entryID uint64
		entries []domain.PurgePreviewSubtreeEntry
	}{
		{"empty", root.ID, nil},
		{"invalid ID", 0, []domain.PurgePreviewSubtreeEntry{root}},
		{"missing target", root.ID, []domain.PurgePreviewSubtreeEntry{descendant}},
		{"target no longer purging", root.ID, []domain.PurgePreviewSubtreeEntry{completed, descendant}},
		{"duplicate target", root.ID, []domain.PurgePreviewSubtreeEntry{root, root}},
		{"duplicate descendant", root.ID, []domain.PurgePreviewSubtreeEntry{root, descendant, descendant}},
		{"invalid descendant", root.ID, []domain.PurgePreviewSubtreeEntry{root, invalid}},
		{"file with descendants", root.ID, []domain.PurgePreviewSubtreeEntry{fileRoot, descendant}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			if count, err := CountPurgeRemainingEntries(scenario.entryID, scenario.entries); count != 0 ||
				!errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常范围应拒绝交付计数", count, err)
			}
		})
	}
}

func TestCountPurgeRemainingEntriesScopeLimit(t *testing.T) {
	entries := make([]domain.PurgePreviewSubtreeEntry, domain.MaxFileEntrySubtreeNodes)
	for index := range entries {
		entries[index] = domain.PurgePreviewSubtreeEntry{
			ID: uint64(index + 1), EntryType: "DIR", Status: "ACTIVE", Revision: 1,
		}
	}
	entries[0].Status = "PURGING"
	if count, err := CountPurgeRemainingEntries(1, entries); err != nil || count != domain.MaxFileEntrySubtreeNodes {
		t.Fatal("范围上限内应返回完整计数", count, err)
	}
	entries = append(entries, domain.PurgePreviewSubtreeEntry{ID: uint64(len(entries) + 1), EntryType: "DIR", Status: "PURGED", Revision: 1})
	if count, err := CountPurgeRemainingEntries(1, entries); count != 0 ||
		!errors.Is(err, domain.ErrFileEntrySubtreeTooLarge) {
		t.Fatal("所有保留记录都应计入范围上限", count, err)
	}
}
