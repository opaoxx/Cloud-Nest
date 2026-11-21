package service

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestValidateUploadPublicationTarget(t *testing.T) {
	for _, mode := range []string{"SINGLE", "MULTIPART"} {
		t.Run(mode, func(t *testing.T) {
			_, reader := fileEntryPathFixture()
			reader.entries[13].EntryType, reader.entries[13].FileSize = "DIR", 0
			task := &domain.UploadTask{ID: 19, UserID: 7, TargetParentID: 13, Status: domain.UploadStatusVerifying, UploadMode: mode}
			ctx := t.Context()
			path, err := ValidateUploadPublicationTarget(ctx, task, reader)
			if err != nil || path == nil || len(path.Entries) != 3 || !reflect.DeepEqual(reader.entryIDs, []uint64{13, 12, 11}) ||
				path.Entries[0].ID != 11 || path.Entries[2].ID != 13 {
				t.Fatal("目标复核未按预期读取完整父链", path, err)
			}
			for index, queryContext := range reader.contexts {
				if queryContext != ctx || reader.userIDs[index] != task.UserID {
					t.Fatal("路径读取没有复用事务上下文及任务用户")
				}
			}
			*path.Entries[2].ParentID = 99
			if *reader.entries[13].ParentID != 12 {
				t.Fatal("复核结果共享了读入记录的父目录指针")
			}
		})
	}
}

func TestValidateUploadPublicationTargetErrors(t *testing.T) {
	readFailure := errors.New("fixture path read failed")
	for _, scenario := range []struct {
		name      string
		edit      func(*domain.UploadTask, *fileEntryPathReaderStub)
		cancel    bool
		wantError error
	}{
		{name: "missing target ID", edit: func(task *domain.UploadTask, _ *fileEntryPathReaderStub) { task.TargetParentID = 0 }, wantError: domain.ErrInvalidUploadParentID},
		{name: "wrong stage", edit: func(task *domain.UploadTask, _ *fileEntryPathReaderStub) { task.Status = domain.UploadStatusCompleted }, wantError: domain.ErrUploadTaskNotReady},
		{name: "unsupported upload mode", edit: func(task *domain.UploadTask, _ *fileEntryPathReaderStub) { task.UploadMode = "UNKNOWN" }, wantError: domain.ErrUploadTaskNotReady},
		{name: "target is file", edit: func(task *domain.UploadTask, _ *fileEntryPathReaderStub) { task.TargetParentID = 13 }, wantError: domain.ErrUploadTargetUnavailable},
		{name: "target missing", edit: func(_ *domain.UploadTask, reader *fileEntryPathReaderStub) { delete(reader.entries, 12) }, wantError: domain.ErrUploadTargetUnavailable},
		{name: "target trashed", edit: func(_ *domain.UploadTask, reader *fileEntryPathReaderStub) { reader.entries[12].Status = "TRASHED" }, wantError: domain.ErrUploadTargetUnavailable},
		{name: "path cycle", edit: func(_ *domain.UploadTask, reader *fileEntryPathReaderStub) {
			directory := reader.entries[12]
			directory.ParentID = &directory.ID
		}, wantError: domain.ErrInvalidFileEntryData},
		{name: "query failure", edit: func(_ *domain.UploadTask, reader *fileEntryPathReaderStub) { reader.entryErrors[12] = readFailure }, wantError: readFailure},
		{name: "query canceled", cancel: true, wantError: context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, reader := fileEntryPathFixture()
			task := &domain.UploadTask{ID: 19, UserID: 7, TargetParentID: 12, Status: domain.UploadStatusVerifying, UploadMode: "SINGLE"}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if scenario.edit != nil {
				scenario.edit(task, reader)
			}
			if scenario.cancel {
				reader.afterEntry = func(uint64) { cancel() }
			}
			path, err := ValidateUploadPublicationTarget(ctx, task, reader)
			if path != nil || !errors.Is(err, scenario.wantError) {
				t.Fatal("发布目标复核错误不符", path, err)
			}
		})
	}
}
