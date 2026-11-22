package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestValidateFileReuseTargetUsesTransactionReader(t *testing.T) {
	actor, reader := fileEntryPathFixture()
	ctx := t.Context()
	path, err := ValidateFileReuseTarget(ctx, actor, 12, reader)
	if err != nil || path == nil || len(path.Entries) != 2 || path.Entries[0].ID != 11 || path.Entries[1].ID != 12 ||
		!slices.Equal(reader.entryIDs, []uint64{12, 11}) {
		t.Fatal("没有读到从真实根目录到目标的完整路径", path, err)
	}
	for index, queryContext := range reader.contexts {
		if queryContext != ctx || reader.userIDs[index] != actor.UserID {
			t.Fatal("路径读取改变了事务上下文或账号范围")
		}
	}
}

func TestValidateFileReuseTargetRejectsInvalidInputsAndReads(t *testing.T) {
	readFailure := errors.New("fixture directory read failure")
	for _, scenario := range []struct {
		name          string
		targetID      uint64
		before        func(*domain.Actor, *fileEntryPathReaderStub, context.CancelFunc)
		wantError     error
		wantNoQueries bool
	}{
		{name: "zero parent ID", wantNoQueries: true},
		{name: "missing session", targetID: 12, wantNoQueries: true, before: func(actor *domain.Actor, _ *fileEntryPathReaderStub, _ context.CancelFunc) {
			actor.SessionID = 0
		}},
		{name: "pre-canceled", targetID: 12, wantNoQueries: true, wantError: context.Canceled,
			before: func(_ *domain.Actor, _ *fileEntryPathReaderStub, cancel context.CancelFunc) { cancel() }},
		{name: "canceled during read", targetID: 12, wantError: context.Canceled, before: func(_ *domain.Actor, reader *fileEntryPathReaderStub, cancel context.CancelFunc) {
			reader.afterEntry = func(uint64) { cancel() }
		}},
		{name: "reader failed", targetID: 12, wantError: readFailure, before: func(_ *domain.Actor, reader *fileEntryPathReaderStub, _ context.CancelFunc) {
			reader.entryErrors[12] = readFailure
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			actor, reader := fileEntryPathFixture()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario.before != nil {
				scenario.before(&actor, reader, cancel)
			}
			path, err := ValidateFileReuseTarget(ctx, actor, scenario.targetID, reader)
			if path != nil || err == nil || (scenario.wantError != nil && !errors.Is(err, scenario.wantError)) {
				t.Fatal("未交付预期的秒传目标错误", path, err)
			}
			if scenario.wantNoQueries && len(reader.contexts) != 0 {
				t.Fatal("输入无效或取消后仍查询了目录")
			}
		})
	}
}
