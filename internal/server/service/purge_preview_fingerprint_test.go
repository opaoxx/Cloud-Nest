package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestBuildPurgePreviewSubtreeFingerprintVectors(t *testing.T) {
	for _, scenario := range []struct {
		name, want string
		entries    []domain.PurgePreviewSubtreeEntry
	}{
		{
			name: "single trashed directory",
			want: "63cecc352d7f4a067024c053582e591e5de4e65cfeb951a4e349e442b7c2e763",
			entries: []domain.PurgePreviewSubtreeEntry{
				{ID: 14, EntryType: "DIR", Status: "TRASHED", Revision: 1},
			},
		},
		{
			name: "all lifecycle states",
			want: "bc153a0f7df0fe9c078dab05127989f8c445b325f611c4989b3a506c2e5e921e",
			entries: []domain.PurgePreviewSubtreeEntry{
				{ID: 14, EntryType: "DIR", Status: "TRASHED", Revision: 1},
				{ID: 8, EntryType: "FILE", Status: "PURGED", Revision: 7, FileSize: 1024},
				{ID: 9, EntryType: "DIR", Status: "TRASHED", Revision: 1},
				{ID: 10, EntryType: "FILE", Status: "TRASHED", Revision: 1, FileSize: 2048},
				{ID: 15, EntryType: "FILE", Status: "ACTIVE", Revision: 5, FileSize: 2048},
				{ID: 17, EntryType: "DIR", Status: "PURGING", Revision: 1},
			},
		},
		{
			name: "full unsigned identifiers and revisions",
			want: "5571c19166f47534a003eb619b9a05888d985dad936dccf4eed40582678fb997",
			entries: []domain.PurgePreviewSubtreeEntry{
				{ID: math.MaxUint64, EntryType: "DIR", Status: "TRASHED", Revision: math.MaxUint64 - 1},
				{ID: math.MaxUint64 - 1, EntryType: "FILE", Status: "PURGED", Revision: math.MaxUint64},
				{ID: 9, EntryType: "FILE", Status: "ACTIVE", Revision: 9007199254740993},
			},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 核对独立 Python 计算的固定向量及输入顺序。
			before := slices.Clone(scenario.entries)
			fingerprint, err := BuildPurgePreviewSubtreeFingerprint(scenario.entries)
			if err != nil || hex.EncodeToString(fingerprint[:]) != scenario.want || !slices.Equal(before, scenario.entries) {
				t.Fatalf("指纹为 %x，期望 %s，错误 %v", fingerprint, scenario.want, err)
			}

			// 2. 相同记录的另一种顺序应得到完全相同的指纹。
			slices.Reverse(before)
			reversed, err := BuildPurgePreviewSubtreeFingerprint(before)
			if err != nil || fingerprint != reversed {
				t.Fatalf("重排后的指纹为 %x，错误 %v", reversed, err)
			}
		})
	}
}

func TestBuildPurgePreviewSubtreeFingerprintDetectsChanges(t *testing.T) {
	root := domain.PurgePreviewSubtreeEntry{ID: 14, EntryType: "DIR", Status: "TRASHED", Revision: 1}
	child := domain.PurgePreviewSubtreeEntry{ID: 15, EntryType: "FILE", Status: "TRASHED", Revision: 2, FileSize: 2048}
	tombstone := domain.PurgePreviewSubtreeEntry{ID: 18, EntryType: "FILE", Status: "PURGED", Revision: 3, FileSize: 1024}
	original, err := BuildPurgePreviewSubtreeFingerprint([]domain.PurgePreviewSubtreeEntry{root, child, tombstone})
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name   string
		change func([]domain.PurgePreviewSubtreeEntry) []domain.PurgePreviewSubtreeEntry
	}{
		{"replaced child ID", func(entries []domain.PurgePreviewSubtreeEntry) []domain.PurgePreviewSubtreeEntry {
			entries[1].ID = 16
			return entries
		}},
		{"updated revision", func(entries []domain.PurgePreviewSubtreeEntry) []domain.PurgePreviewSubtreeEntry {
			entries[1].Revision++
			return entries
		}},
		{"different status", func(entries []domain.PurgePreviewSubtreeEntry) []domain.PurgePreviewSubtreeEntry {
			entries[1].Status = "PURGING"
			return entries
		}},
		{"removed retained tombstone", func(entries []domain.PurgePreviewSubtreeEntry) []domain.PurgePreviewSubtreeEntry {
			return entries[:2]
		}},
		{"added retained tombstone", func(entries []domain.PurgePreviewSubtreeEntry) []domain.PurgePreviewSubtreeEntry {
			added := tombstone
			added.ID++
			return append(entries, added)
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result, err := BuildPurgePreviewSubtreeFingerprint(scenario.change([]domain.PurgePreviewSubtreeEntry{root, child, tombstone}))
			if err != nil || original == result {
				t.Fatal("目录项范围变化未反映到指纹", err)
			}
		})
	}
}

func TestBuildPurgePreviewSubtreeFingerprintRejectsInvalidInput(t *testing.T) {
	target := domain.PurgePreviewSubtreeEntry{ID: 14, EntryType: "DIR", Status: "TRASHED", Revision: 1}
	for _, scenario := range []struct {
		name    string
		entries []domain.PurgePreviewSubtreeEntry
		want    error
	}{
		{"empty", nil, domain.ErrInvalidFileEntryData},
		{"duplicate ID", []domain.PurgePreviewSubtreeEntry{target, {ID: 14, EntryType: "FILE", Status: "PURGED", Revision: 2}}, domain.ErrInvalidFileEntryData},
		{"zero ID", []domain.PurgePreviewSubtreeEntry{target, {EntryType: "FILE", Status: "ACTIVE", Revision: 1}}, domain.ErrInvalidFileEntryData},
		{"zero revision", []domain.PurgePreviewSubtreeEntry{target, {ID: 15, EntryType: "DIR", Status: "TRASHED"}}, domain.ErrInvalidFileEntryData},
		{"invalid status", []domain.PurgePreviewSubtreeEntry{target, {ID: 15, EntryType: "FILE", Status: "GONE", Revision: 1}}, domain.ErrInvalidFileEntryData},
		{"too many records", make([]domain.PurgePreviewSubtreeEntry, domain.MaxFileEntrySubtreeNodes+1), domain.ErrFileEntrySubtreeTooLarge},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result, err := BuildPurgePreviewSubtreeFingerprint(scenario.entries)
			if result != ([sha256.Size]byte{}) || !errors.Is(err, scenario.want) {
				t.Fatalf("无效输入的指纹为 %x，错误 %v，期望 %v", result, err, scenario.want)
			}
		})
	}
}
