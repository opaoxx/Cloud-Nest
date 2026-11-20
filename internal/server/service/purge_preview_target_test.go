package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestValidatePurgePreviewTargetEligible(t *testing.T) {
	for _, scenario := range []struct {
		entryID uint64
		reads   []uint64
	}{
		{13, []uint64{13, 12, 16, 11}},
		{12, []uint64{12, 16, 11}},
	} {
		actor, _, reader := restoreEntryAncestorsFixture()
		reader.entries[scenario.entryID].Status = "TRASHED"
		ctx := t.Context()

		// 1. 按原父链读到真实根目录，每次读取使用相同快照和归属。
		target, err := ValidatePurgePreviewTarget(ctx, actor, scenario.entryID, reader)
		if err != nil || target == nil || target.ID != scenario.entryID || target.UserID != actor.UserID ||
			target.Status != "TRASHED" || target.Revision != reader.entries[scenario.entryID].Revision ||
			!slices.Equal(reader.entryIDs, scenario.reads) {
			t.Fatalf("预检目标 %+v，读取顺序 %v，错误 %v", target, reader.entryIDs, err)
		}
		for index, readContext := range reader.contexts {
			if readContext != ctx || reader.userIDs[index] != actor.UserID {
				t.Fatal("预检读取未使用同一上下文和用户范围")
			}
		}

		// 2. 返回目标及其父目录 ID 的独立副本。
		*target.ParentID = 99
		if *reader.entries[scenario.entryID].ParentID == 99 {
			t.Fatal("目标副本与快照记录共享了父目录字段")
		}
	}
}

func TestValidatePurgePreviewTargetRejections(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		entryID uint64
		change  func(*fileEntryPathReaderStub)
		want    error
	}{
		{name: "root", entryID: 11, want: domain.ErrRootEntryPurge},
		{name: "missing", entryID: 99, want: domain.ErrFileEntryNotFound},
		{name: "active target", entryID: 12, want: domain.ErrFileEntryNotFound},
		{"purging target", 13, func(reader *fileEntryPathReaderStub) { reader.entries[13].Status = "PURGING" }, domain.ErrFileEntryNotFound},
		{"purged target", 13, func(reader *fileEntryPathReaderStub) { reader.entries[13].Status = "PURGED" }, domain.ErrFileEntryNotFound},
		{"trashed parent", 13, func(reader *fileEntryPathReaderStub) { reader.entries[12].Status = "TRASHED" }, domain.ErrFileEntryNotFound},
		{"trashed grandparent", 13, func(reader *fileEntryPathReaderStub) { reader.entries[16].Status = "TRASHED" }, domain.ErrFileEntryNotFound},
		{"purging parent", 13, func(reader *fileEntryPathReaderStub) { reader.entries[12].Status = "PURGING" }, domain.ErrFileEntryNotFound},
		{"purged grandparent", 13, func(reader *fileEntryPathReaderStub) { reader.entries[16].Status = "PURGED" }, domain.ErrFileEntryNotFound},
		{"wrong owner in reader result", 13, func(reader *fileEntryPathReaderStub) { reader.entries[13].UserID++ }, domain.ErrInvalidFileEntryData},
		{"ancestor is file", 13, func(reader *fileEntryPathReaderStub) { reader.entries[12].EntryType = "FILE" }, domain.ErrInvalidFileEntryData},
		{"root has wrong status", 13, func(reader *fileEntryPathReaderStub) { reader.entries[11].Status = "TRASHED" }, domain.ErrInvalidFileEntryData},
		{"missing ancestor", 13, func(reader *fileEntryPathReaderStub) { delete(reader.entries, 16) }, domain.ErrInvalidFileEntryData},
		{"broken chain beyond trashed ancestor", 13, func(reader *fileEntryPathReaderStub) {
			reader.entries[12].Status = "TRASHED"
			delete(reader.entries, 16)
		}, domain.ErrInvalidFileEntryData},
		{"cycle", 13, func(reader *fileEntryPathReaderStub) { reader.entries[16].ParentID = &reader.entries[12].ID }, domain.ErrInvalidFileEntryData},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actor, _, reader := restoreEntryAncestorsFixture()
			if scenario.change != nil {
				scenario.change(reader)
			}
			target, err := ValidatePurgePreviewTarget(t.Context(), actor, scenario.entryID, reader)
			if target != nil || !errors.Is(err, scenario.want) {
				t.Fatalf("预检目标 %+v，错误 %v，期望 %v", target, err, scenario.want)
			}
		})
	}
}

func TestValidatePurgePreviewTargetCancellation(t *testing.T) {
	actor, _, reader := restoreEntryAncestorsFixture()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader.afterEntry = func(entryID uint64) {
		if entryID == 12 {
			cancel()
		}
	}
	target, err := ValidatePurgePreviewTarget(ctx, actor, 13, reader)
	if target != nil || !errors.Is(err, context.Canceled) || !slices.Equal(reader.entryIDs, []uint64{13, 12}) {
		t.Fatalf("取消后目标 %+v，读取 %v，错误 %v", target, reader.entryIDs, err)
	}
}
