package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestCheckTrashEntryVisibilityPaths(t *testing.T) {
	for _, parentID := range []uint64{11, 12} {
		t.Run(fmt.Sprint(parentID), func(t *testing.T) {
			actor, reader := fileEntryPathFixture()
			candidate := domain.TrashedFileEntry{FileEntry: *reader.entries[13], DeletedAt: reader.entries[13].ModifiedAt}
			candidate.Status, candidate.ParentID = "TRASHED", &parentID
			reader.entries[13] = &candidate.FileEntry

			// 1. 路径只读取原父目录及其祖先，返回顺序从真实根目录开始。
			path, err := CheckTrashEntryVisibility(t.Context(), actor, candidate, reader)
			if err != nil || path == nil || len(path.Entries) != int(parentID-10) ||
				path.Entries[0].ID != 11 || path.Entries[len(path.Entries)-1].ID != parentID {
				t.Fatalf("原父路径为 %+v，错误为 %v", path, err)
			}
			expectedReads := []uint64{11}
			if parentID == 12 {
				expectedReads = []uint64{12, 11}
				// 2. 路径中的父目录指针和名称有自己的副本。
				reader.entries[12].Name = "读取之后改名"
				*path.Entries[1].ParentID = 99
				if path.Entries[1].Name != "课程资料 🌱" || *reader.entries[12].ParentID != 11 {
					t.Fatal("返回路径和读取记录共享了可修改字段")
				}
			}
			if !slices.Equal(reader.entryIDs, expectedReads) {
				t.Fatalf("实际读取 %v，期望 %v", reader.entryIDs, expectedReads)
			}
		})
	}
}

func TestCheckTrashEntryVisibilityHidden(t *testing.T) {
	for _, status := range []string{"TRASHED", "PURGING", "PURGED"} {
		for _, hiddenID := range []uint64{12, 14} {
			t.Run(fmt.Sprintf("%s %d", status, hiddenID), func(t *testing.T) {
				actor, reader := fileEntryPathFixture()
				parent, grandparent := uint64(14), uint64(12)
				directory := *reader.entries[12]
				directory.ID, directory.ParentID = parent, &grandparent
				reader.entries[parent] = &directory
				reader.entries[hiddenID].Status = status
				candidate := domain.TrashedFileEntry{FileEntry: *reader.entries[13], DeletedAt: reader.entries[13].ModifiedAt}
				candidate.Status, candidate.ParentID = "TRASHED", &parent

				// 1. 父目录和更上层祖先都可能把候选隐藏。
				path, err := CheckTrashEntryVisibility(t.Context(), actor, candidate, reader)
				if path != nil || err != nil || len(reader.entryIDs) == 0 || reader.entryIDs[len(reader.entryIDs)-1] != hiddenID {
					t.Fatalf("隐藏候选返回了路径或错误，路径为 %+v，错误为 %v", path, err)
				}
			})
		}
	}
}

func TestCheckTrashEntryVisibilityFailures(t *testing.T) {
	readFailure := errors.New("祖先读取失败")
	invalid := domain.ErrInvalidFileEntryData
	for _, testCase := range []struct {
		name   string
		change func(*domain.TrashedFileEntry, *fileEntryPathReaderStub)
		want   error
	}{
		{"foreign candidate", func(candidate *domain.TrashedFileEntry, _ *fileEntryPathReaderStub) { candidate.UserID = 8 }, invalid},
		{"active candidate", func(candidate *domain.TrashedFileEntry, _ *fileEntryPathReaderStub) { candidate.Status = "ACTIVE" }, invalid},
		{"missing deletion time", func(candidate *domain.TrashedFileEntry, _ *fileEntryPathReaderStub) {
			candidate.DeletedAt = time.Time{}
		}, invalid},
		{"no parent", func(candidate *domain.TrashedFileEntry, _ *fileEntryPathReaderStub) { candidate.ParentID = nil }, invalid},
		{"self parent", func(candidate *domain.TrashedFileEntry, _ *fileEntryPathReaderStub) {
			candidate.ParentID = &candidate.ID
		}, invalid},
		{"root candidate", func(candidate *domain.TrashedFileEntry, _ *fileEntryPathReaderStub) { candidate.ID = 11 }, invalid},
		{"missing parent", func(_ *domain.TrashedFileEntry, reader *fileEntryPathReaderStub) { delete(reader.entries, 12) }, invalid},
		{"missing grandparent", func(_ *domain.TrashedFileEntry, reader *fileEntryPathReaderStub) { delete(reader.entries, 11) }, invalid},
		{"foreign parent", func(_ *domain.TrashedFileEntry, reader *fileEntryPathReaderStub) { reader.entries[12].UserID = 8 }, invalid},
		{"file parent", func(_ *domain.TrashedFileEntry, reader *fileEntryPathReaderStub) {
			reader.entries[12].EntryType = "FILE"
		}, invalid},
		{"hidden file parent", func(_ *domain.TrashedFileEntry, reader *fileEntryPathReaderStub) {
			reader.entries[12].EntryType, reader.entries[12].Status = "FILE", "TRASHED"
		}, invalid},
		{"hidden root", func(_ *domain.TrashedFileEntry, reader *fileEntryPathReaderStub) {
			reader.entries[11].Status = "TRASHED"
		}, invalid},
		{"cycle through candidate", func(candidate *domain.TrashedFileEntry, reader *fileEntryPathReaderStub) {
			reader.entries[12].ParentID = &candidate.ID
		}, invalid},
		{"disabled user", func(_ *domain.TrashedFileEntry, reader *fileEntryPathReaderStub) { reader.profile.Status = "DISABLED" }, domain.ErrAccountDisabled},
		{"profile read error", func(_ *domain.TrashedFileEntry, reader *fileEntryPathReaderStub) {
			reader.profileError = domain.ErrFileEntryNotFound
		}, domain.ErrFileEntryNotFound},
		{"ancestor read error", func(_ *domain.TrashedFileEntry, reader *fileEntryPathReaderStub) {
			reader.entryErrors[12] = readFailure
		}, readFailure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 异常候选、异常父链和读取故障都返回具体原因。
			actor, reader := fileEntryPathFixture()
			candidate := domain.TrashedFileEntry{FileEntry: *reader.entries[13], DeletedAt: reader.entries[13].ModifiedAt}
			candidate.Status = "TRASHED"
			testCase.change(&candidate, reader)
			path, err := CheckTrashEntryVisibility(t.Context(), actor, candidate, reader)
			if path != nil || !errors.Is(err, testCase.want) {
				t.Fatalf("原父路径为 %+v，错误为 %v，期望 %v", path, err, testCase.want)
			}
		})
	}
	t.Run("canceled while reading ancestor", func(t *testing.T) {
		actor, reader := fileEntryPathFixture()
		candidate := domain.TrashedFileEntry{FileEntry: *reader.entries[13], DeletedAt: reader.entries[13].ModifiedAt}
		candidate.Status = "TRASHED"
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		reader.afterEntry = func(uint64) { cancel() }
		if path, err := CheckTrashEntryVisibility(ctx, actor, candidate, reader); path != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("取消时路径为 %+v，错误为 %v", path, err)
		}
	})
}

func TestCheckTrashEntryVisibilityDepth(t *testing.T) {
	for _, parentDepth := range []int{domain.MaxFileEntryDepth - 1, domain.MaxFileEntryDepth} {
		t.Run(fmt.Sprint(parentDepth), func(t *testing.T) {
			// 1. 构造两条相邻深度的父链，验证候选自身也占一层。
			actor, reader := fileEntryPathFixture()
			parentID := uint64(11)
			for level := 1; level <= parentDepth; level++ {
				directory := *reader.entries[11]
				ancestorID := parentID
				directory.ID, directory.ParentID, directory.Name = uint64(100+level), &ancestorID, fmt.Sprintf("第 %d 层", level)
				reader.entries[directory.ID] = &directory
				parentID = directory.ID
			}
			candidate := domain.TrashedFileEntry{FileEntry: *reader.entries[13], DeletedAt: reader.entries[13].ModifiedAt}
			candidate.Status, candidate.ParentID = "TRASHED", &parentID
			path, err := CheckTrashEntryVisibility(t.Context(), actor, candidate, reader)
			if parentDepth < domain.MaxFileEntryDepth {
				if err != nil || path == nil || len(path.Entries) != domain.MaxFileEntryDepth {
					t.Fatalf("第 32 层候选的原父路径为 %+v，错误为 %v", path, err)
				}
			} else if path != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) || len(reader.entryIDs) != domain.MaxFileEntryDepth {
				t.Fatalf("超过深度上限的路径为 %+v，读取 %v，错误为 %v", path, reader.entryIDs, err)
			}
		})
	}
}
