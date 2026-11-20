package service

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestReadRestoreEntryTarget(t *testing.T) {
	actor, reader := fileEntryPathFixture()
	reader.entries[12].Status = "TRASHED"
	reader.entries[13].Status = "TRASHED"

	// 1. 读取本次目标，保留原父目录供接下来的祖先检查。
	target, err := ReadRestoreEntryTarget(t.Context(), actor, 13, reader)
	if err != nil || target == nil || target.ID != 13 || target.UserID != actor.UserID ||
		target.ParentID == nil || *target.ParentID != 12 || target.Status != "TRASHED" ||
		!slices.Equal(reader.entryIDs, []uint64{13}) || !slices.Equal(reader.userIDs, []uint64{actor.UserID, actor.UserID}) {
		t.Fatalf("回收目标为 %+v，读取 ID 为 %v，错误为 %v", target, reader.entryIDs, err)
	}
	// 2. 目标的父目录指针可以独立使用。
	*target.ParentID = 99
	if *reader.entries[13].ParentID != 12 {
		t.Fatal("目标和读取记录共享可变父目录指针")
	}
}

func TestReadRestoreEntryTargetRejections(t *testing.T) {
	readFailure := errors.New("目标读取失败")
	invalid := domain.ErrInvalidFileEntryData
	for _, testCase := range []struct {
		name    string
		entryID uint64
		change  func(*fileEntryPathReaderStub)
		want    error
	}{
		{name: "real root", entryID: 11, want: domain.ErrRootEntryRestore},
		{name: "missing target", entryID: 99, want: domain.ErrFileEntryNotFound},
		{name: "active target", entryID: 12, want: domain.ErrFileEntryNotFound},
		{"purging target", 13, func(reader *fileEntryPathReaderStub) { reader.entries[13].Status = "PURGING" }, domain.ErrFileEntryNotFound},
		{"purged target", 13, func(reader *fileEntryPathReaderStub) { reader.entries[13].Status = "PURGED" }, domain.ErrFileEntryNotFound},
		{"invalid status", 13, func(reader *fileEntryPathReaderStub) { reader.entries[13].Status = "INVALID" }, invalid},
		{"foreign data returned", 13, func(reader *fileEntryPathReaderStub) { reader.entries[13].UserID++ }, invalid},
		{"wrong identifier returned", 13, func(reader *fileEntryPathReaderStub) { reader.entries[13].ID++ }, invalid},
		{"zero revision", 13, func(reader *fileEntryPathReaderStub) { reader.entries[13].Revision = 0 }, invalid},
		{"ordinary target without a parent", 13, func(reader *fileEntryPathReaderStub) { reader.entries[13].ParentID = nil }, invalid},
		{"self parent", 13, func(reader *fileEntryPathReaderStub) { reader.entries[13].ParentID = &reader.entries[13].ID }, invalid},
		{"invalid name", 13, func(reader *fileEntryPathReaderStub) { reader.entries[13].Name = "\xff" }, invalid},
		{"invalid type", 13, func(reader *fileEntryPathReaderStub) { reader.entries[13].EntryType = "UNKNOWN" }, invalid},
		{"invalid root shape", 11, func(reader *fileEntryPathReaderStub) { reader.entries[11].Name = "有名称的根目录" }, invalid},
		{"disabled user", 13, func(reader *fileEntryPathReaderStub) { reader.profile.Status = "DISABLED" }, domain.ErrAccountDisabled},
		{"missing root", 13, func(reader *fileEntryPathReaderStub) { reader.profile.RootEntryID = 0 }, domain.ErrInvalidAccountData},
		{"query error", 13, func(reader *fileEntryPathReaderStub) { reader.entryErrors[13] = readFailure }, readFailure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, reader := fileEntryPathFixture()
			reader.entries[13].Status = "TRASHED"
			if testCase.change != nil {
				testCase.change(reader)
			}
			// 1. 拒绝异常目标并保留错误类别。
			target, err := ReadRestoreEntryTarget(t.Context(), actor, testCase.entryID, reader)
			if target != nil || !errors.Is(err, testCase.want) {
				t.Fatalf("恢复目标为 %+v，错误为 %v，期望 %v", target, err, testCase.want)
			}
		})
	}
}

func TestReadRestoreEntryTargetCancellation(t *testing.T) {
	actor, reader := fileEntryPathFixture()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader.afterEntry = func(uint64) { cancel() }
	reader.entryErrors[13] = domain.ErrFileEntryNotFound

	// 1. 目标查询途中取消时停止后续读取。
	target, err := ReadRestoreEntryTarget(ctx, actor, 13, reader)
	if target != nil || !errors.Is(err, context.Canceled) || len(reader.userIDs) != 1 {
		t.Fatalf("取消后返回目标 %+v，读取 %v，错误为 %v", target, reader.userIDs, err)
	}
}

func TestValidateRestoreEntryRevision(t *testing.T) {
	for _, testCase := range []struct {
		name              string
		current, expected uint64
		want              error
	}{
		{"matching", 3, 3, nil},
		{"last incrementable value", math.MaxUint64 - 1, math.MaxUint64 - 1, nil},
		{"stale version", 4, 3, domain.ErrEntryRevisionConflict},
		{"stale version at upper limit", math.MaxUint64, math.MaxUint64 - 1, domain.ErrEntryRevisionConflict},
		{"overflow on increment", math.MaxUint64, math.MaxUint64, domain.ErrEntryRevisionExhausted},
		{"invalid stored version", 0, 3, domain.ErrInvalidFileEntryData},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := ValidateRestoreEntryRevision(testCase.current, testCase.expected)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("版本检查错误为 %v，期望 %v", err, testCase.want)
			}
			if testCase.want == domain.ErrEntryRevisionConflict {
				var conflict *domain.EntryRevisionConflictError
				if !errors.As(err, &conflict) || conflict.CurrentRevision != testCase.current {
					t.Fatalf("版本冲突未保留当前版本 %d，错误为 %v", testCase.current, err)
				}
			}
		})
	}
	if err := ValidateRestoreEntryRevision(3, 0); err == nil {
		t.Fatal("零预期版本通过校验")
	}
}
