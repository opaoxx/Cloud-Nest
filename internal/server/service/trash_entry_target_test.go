package service

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestValidateTrashEntryTarget(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		entryID    uint64
		revision   uint64
		entryChain []uint64
	}{
		{"ordinary directory", 12, 1, []uint64{12, 11}},
		{"nested file", 13, 3, []uint64{13, 12, 11}},
		{"last incrementable revision", 13, math.MaxUint64 - 1, []uint64{13, 12, 11}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 用调用方的事务读取上下文检查完整目标路径。
			actor, reader := fileEntryPathFixture()
			source := reader.entries[scenario.entryID]
			source.Revision = scenario.revision
			ctx := t.Context()
			result, err := ValidateTrashEntryTarget(ctx, actor, source.ID, scenario.revision, reader)
			if err != nil || result == nil || !reflect.DeepEqual(result, source) ||
				!reflect.DeepEqual(reader.entryIDs, scenario.entryChain) {
				t.Fatal("回收目标或路径读取不符", result, err)
			}
			for index, queryContext := range reader.contexts {
				if queryContext != ctx || reader.userIDs[index] != actor.UserID {
					t.Fatal("查询未复用事务上下文和用户范围")
				}
			}

			// 2. 交付的目录项与读取器持有的值相互独立。
			originalParent := *source.ParentID
			*result.ParentID, result.Name = 99, "本地修改"
			if *source.ParentID != originalParent || source.Name == result.Name {
				t.Fatal("修改检查结果影响了原始目录项")
			}
		})
	}
}

func TestValidateTrashEntryTargetRejections(t *testing.T) {
	readError := errors.New("fixture ancestor read failed")
	for _, scenario := range []struct {
		name          string
		entryID       uint64
		revision      uint64
		edit          func(*fileEntryPathReaderStub)
		want          error
		actualVersion uint64
	}{
		{name: "root before version", entryID: 11, revision: 2, want: domain.ErrRootEntryTrash},
		{name: "missing target", entryID: 99, revision: 3, want: domain.ErrFileEntryNotFound},
		{name: "trashed target", edit: func(reader *fileEntryPathReaderStub) { reader.entries[13].Status = "TRASHED" }, want: domain.ErrFileEntryNotFound},
		{name: "trashed ancestor before version", revision: 8, edit: func(reader *fileEntryPathReaderStub) { reader.entries[12].Status = "TRASHED" }, want: domain.ErrFileEntryNotFound},
		{name: "purging ancestor", edit: func(reader *fileEntryPathReaderStub) { reader.entries[12].Status = "PURGING" }, want: domain.ErrFileEntryNotFound},
		{name: "purged ancestor", edit: func(reader *fileEntryPathReaderStub) { reader.entries[12].Status = "PURGED" }, want: domain.ErrFileEntryNotFound},
		{name: "broken parent chain", edit: func(reader *fileEntryPathReaderStub) { delete(reader.entries, 12) }, want: domain.ErrInvalidFileEntryData},
		{name: "invalid scoped result", edit: func(reader *fileEntryPathReaderStub) { reader.entries[13].UserID = 8 }, want: domain.ErrInvalidFileEntryData},
		{name: "query failure", edit: func(reader *fileEntryPathReaderStub) { reader.entryErrors[12] = readError }, want: readError},
		{name: "stale version", revision: 2, want: domain.ErrEntryRevisionConflict, actualVersion: 3},
		{name: "exhausted revision", revision: math.MaxUint64, edit: func(reader *fileEntryPathReaderStub) {
			reader.entries[13].Revision = math.MaxUint64
		}, want: domain.ErrEntryRevisionExhausted},
		{name: "conflict before exhaustion", edit: func(reader *fileEntryPathReaderStub) {
			reader.entries[13].Revision = math.MaxUint64
		}, want: domain.ErrEntryRevisionConflict, actualVersion: math.MaxUint64},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actor, reader := fileEntryPathFixture()
			entryID, revision := scenario.entryID, scenario.revision
			if entryID == 0 {
				entryID = 13
			}
			if revision == 0 {
				revision = 3
			}
			if scenario.edit != nil {
				scenario.edit(reader)
			}
			// 1. 确认拒绝时没有可供写入的目标，并保留失败原因。
			result, err := ValidateTrashEntryTarget(t.Context(), actor, entryID, revision, reader)
			if result != nil || !errors.Is(err, scenario.want) {
				t.Fatal("回收目标拒绝结果不符", result, err)
			}
			// 2. 版本冲突交付真实版本，隐藏路径和根目录错误保留各自类型。
			var conflict *domain.EntryRevisionConflictError
			if errors.As(err, &conflict) != (scenario.actualVersion != 0) ||
				(conflict != nil && conflict.CurrentRevision != scenario.actualVersion) {
				t.Fatal("回收目标错误优先级或实际版本不符", err)
			}
		})
	}
}

func TestValidateTrashEntryTargetInputAndCancellation(t *testing.T) {
	for _, scenario := range []string{"nil context", "nil reader", "zero user", "zero device", "zero session", "zero target", "zero revision", "already canceled", "cancel while reading"} {
		t.Run(scenario, func(t *testing.T) {
			actor, stub := fileEntryPathFixture()
			callContext, cancel := context.WithCancel(t.Context())
			defer cancel()
			var ctx context.Context = callContext
			var reader FileEntryPathReader = stub
			entryID, revision := uint64(13), uint64(3)
			var wantError error
			expectedReads := 0
			switch scenario {
			case "nil context":
				ctx = nil
			case "nil reader":
				reader = nil
			case "zero user":
				actor.UserID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero session":
				actor.SessionID = 0
			case "zero target":
				entryID = 0
			case "zero revision":
				revision = 0
			case "already canceled":
				cancel()
				wantError = context.Canceled
			case "cancel while reading":
				stub.afterEntry = func(uint64) { cancel() }
				wantError = context.Canceled
				expectedReads = 2 // 账号资料和目标。
			}
			// 1. 无效输入在查询前停止，读到取消后停止后续祖先查询。
			result, err := ValidateTrashEntryTarget(ctx, actor, entryID, revision, reader)
			if result != nil || err == nil || (wantError != nil && !errors.Is(err, wantError)) || len(stub.contexts) != expectedReads {
				t.Fatal("回收目标输入或取消边界错误", result, err, len(stub.contexts))
			}
		})
	}
}
