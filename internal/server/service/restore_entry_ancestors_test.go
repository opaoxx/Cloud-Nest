package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func restoreEntryAncestorsFixture() (domain.Actor, *domain.FileEntry, *fileEntryPathReaderStub) {
	actor, reader := fileEntryPathFixture()
	directParent := reader.entries[12]
	grandparent := *directParent
	grandparent.ID, grandparent.Name = 16, "上级目录"
	reader.entries[16] = &grandparent
	directParent.ParentID = &grandparent.ID
	target := reader.entries[13]
	target.Status = "TRASHED"
	return actor, target, reader
}

func TestReadRestoreEntryAncestorsStates(t *testing.T) {
	for _, scenario := range []struct {
		name            string
		parentID        uint64
		parentStatus    string
		upperStatus     string
		wantIDs         []uint64
		wantTrashed     bool
		wantUnavailable bool
	}{
		{name: "direct root", parentID: 11, parentStatus: "ACTIVE", upperStatus: "ACTIVE", wantIDs: []uint64{11}},
		{name: "active path", parentID: 12, parentStatus: "ACTIVE", upperStatus: "ACTIVE", wantIDs: []uint64{12, 16, 11}},
		{name: "trashed direct parent", parentID: 12, parentStatus: "TRASHED", upperStatus: "ACTIVE", wantIDs: []uint64{12, 16, 11}, wantTrashed: true},
		{name: "trashed grandparent", parentID: 12, parentStatus: "ACTIVE", upperStatus: "TRASHED", wantIDs: []uint64{12, 16, 11}, wantTrashed: true},
		{name: "purging above trashed parent", parentID: 12, parentStatus: "TRASHED", upperStatus: "PURGING", wantIDs: []uint64{12, 16, 11}, wantUnavailable: true},
		{name: "purged above trashed parent", parentID: 12, parentStatus: "TRASHED", upperStatus: "PURGED", wantIDs: []uint64{12, 16, 11}, wantUnavailable: true},
		{name: "purging direct parent", parentID: 12, parentStatus: "PURGING", upperStatus: "ACTIVE", wantIDs: []uint64{12, 16, 11}, wantUnavailable: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actor, target, reader := restoreEntryAncestorsFixture()
			target.ParentID = &scenario.parentID
			reader.entries[12].Status, reader.entries[16].Status = scenario.parentStatus, scenario.upperStatus
			ctx := t.Context()
			result, err := ReadRestoreEntryAncestors(ctx, actor, target, reader)
			if !reflect.DeepEqual(reader.entryIDs, scenario.wantIDs) {
				t.Fatalf("父链读取顺序为 %v，期望 %v", reader.entryIDs, scenario.wantIDs)
			}
			for index, readContext := range reader.contexts {
				if readContext != ctx || reader.userIDs[index] != actor.UserID {
					t.Fatal("父链读取没有沿用同一事务上下文和用户")
				}
			}
			if scenario.wantUnavailable {
				if result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
					t.Fatalf("永久清理祖先应禁止恢复，结果为 %+v，错误为 %v", result, err)
				}
				return
			}
			want := RestoreEntryAncestors{RootEntryID: 11, OriginalParentDepth: uint64(len(scenario.wantIDs) - 1), HasTrashedAncestor: scenario.wantTrashed}
			if err != nil || result == nil || *result != want {
				t.Fatalf("父链结果为 %+v，期望 %+v，错误为 %v", result, want, err)
			}
		})
	}
}

func TestReadRestoreEntryAncestorsRejectsInvalidData(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*domain.FileEntry, *fileEntryPathReaderStub)
	}{
		{"foreign target", func(target *domain.FileEntry, _ *fileEntryPathReaderStub) { target.UserID++ }},
		{"active target", func(target *domain.FileEntry, _ *fileEntryPathReaderStub) { target.Status = "ACTIVE" }},
		{"target without parent", func(target *domain.FileEntry, _ *fileEntryPathReaderStub) { target.ParentID = nil }},
		{"target is root", func(target *domain.FileEntry, _ *fileEntryPathReaderStub) { target.ID = 11 }},
		{"missing linked parent", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) { delete(reader.entries, 12) }},
		{"missing root", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) { delete(reader.entries, 11) }},
		{"wrong record", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) { reader.entries[12].ID = 99 }},
		{"wrong owner", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) { reader.entries[16].UserID++ }},
		{"zero revision", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) { reader.entries[12].Revision = 0 }},
		{"file ancestor", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) { reader.entries[12].EntryType = "FILE" }},
		{"directory has file size", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) { reader.entries[12].FileSize = 1 }},
		{"invalid parent name", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) { reader.entries[12].Name = "\xff" }},
		{"unnamed ancestor", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) { reader.entries[12].Name = "" }},
		{"unexpected status", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) { reader.entries[16].Status = "UNKNOWN" }},
		{"unexpected root", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) { reader.entries[16].ParentID = nil }},
		{"named root", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) { reader.entries[11].Name = "假根目录" }},
		{"nonactive root", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) { reader.entries[11].Status = "PURGING" }},
		{"root has parent", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) {
			reader.entries[11].ParentID = &reader.entries[16].ID
		}},
		{"cycle back to target", func(target *domain.FileEntry, reader *fileEntryPathReaderStub) {
			reader.entries[16].ParentID = &target.ID
		}},
		{"ancestor cycle", func(_ *domain.FileEntry, reader *fileEntryPathReaderStub) {
			reader.entries[16].ParentID = &reader.entries[12].ID
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actor, target, reader := restoreEntryAncestorsFixture()
			scenario.mutate(target, reader)
			result, err := ReadRestoreEntryAncestors(t.Context(), actor, target, reader)
			if result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatalf("异常数据应被拒绝，结果为 %+v，错误为 %v", result, err)
			}
		})
	}
}

func TestReadRestoreEntryAncestorsDepthLimit(t *testing.T) {
	for _, targetDepth := range []uint64{domain.MaxFileEntryDepth, domain.MaxFileEntryDepth + 1} {
		actor, target, reader := restoreEntryAncestorsFixture()
		parentID := uint64(11)
		for depth := uint64(1); depth < targetDepth; depth++ {
			entry := &domain.FileEntry{
				ID: 100 + depth, UserID: actor.UserID, ParentID: copyOptionalValue(&parentID),
				Name: "目录", EntryType: "DIR", Status: "ACTIVE", Revision: 1,
			}
			reader.entries[entry.ID] = entry
			parentID = entry.ID
		}
		target.ParentID = &parentID
		result, err := ReadRestoreEntryAncestors(t.Context(), actor, target, reader)
		if len(reader.entryIDs) != domain.MaxFileEntryDepth {
			t.Fatalf("目标深度为 %d 时查询了 %d 个祖先", targetDepth, len(reader.entryIDs))
		}
		if targetDepth == domain.MaxFileEntryDepth {
			if err != nil || result == nil || result.OriginalParentDepth != targetDepth-1 {
				t.Fatalf("深度上限内父链应有效，结果为 %+v，错误为 %v", result, err)
			}
		} else if result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
			t.Fatalf("超深父链应被拒绝，结果为 %+v，错误为 %v", result, err)
		}
	}
}

func TestReadRestoreEntryAncestorsQueryErrorAndCancellation(t *testing.T) {
	actor, target, reader := restoreEntryAncestorsFixture()
	queryError := errors.New("fixture query")
	reader.entryErrors[16] = queryError
	result, err := ReadRestoreEntryAncestors(t.Context(), actor, target, reader)
	if result != nil || !errors.Is(err, queryError) || !reflect.DeepEqual(reader.entryIDs, []uint64{12, 16}) {
		t.Fatalf("查询失败应停止读取并保留原错误，查询为 %v，错误为 %v", reader.entryIDs, err)
	}

	actor, target, reader = restoreEntryAncestorsFixture()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader.afterEntry = func(entryID uint64) {
		if entryID == 12 {
			cancel()
		}
	}
	result, err = ReadRestoreEntryAncestors(ctx, actor, target, reader)
	if result != nil || !errors.Is(err, context.Canceled) || !reflect.DeepEqual(reader.entryIDs, []uint64{12}) {
		t.Fatalf("取消后应停止向上读取，查询为 %v，错误为 %v", reader.entryIDs, err)
	}
}
