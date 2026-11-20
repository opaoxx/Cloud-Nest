package service

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type entryDownloadTargetReaderStub struct {
	*fileEntryPathReaderStub
	readContent func(context.Context, uint64, uint64) (*domain.FileEntryContent, error)
}

func (reader *entryDownloadTargetReaderStub) FindUserFileEntryContent(ctx context.Context, userID, entryID uint64) (*domain.FileEntryContent, error) {
	return reader.readContent(ctx, userID, entryID)
}

func TestEntryDownloadTargetService(t *testing.T) {
	readFailure := errors.New("fixture content read failure")
	snapshotFailure := errors.New("fixture snapshot close failure")
	for _, scenario := range []struct {
		name                   string
		entryID, revision      uint64
		editPath               func(*fileEntryPathReaderStub)
		editContent            func(*domain.FileEntryContent) *domain.FileEntryContent
		readError, commitError error
		wantError              error
		readCalls              int
		cancelInRead           bool
	}{
		{name: "visible file", readCalls: 1},
		{name: "unknown MIME", editContent: func(content *domain.FileEntryContent) *domain.FileEntryContent {
			content.MIMEType = nil
			return content
		}, readCalls: 1},
		{name: "full unsigned revision", revision: math.MaxUint64, editPath: func(reader *fileEntryPathReaderStub) {
			reader.entries[13].Revision = math.MaxUint64
		}, readCalls: 1},
		{name: "missing", entryID: 99, wantError: domain.ErrFileEntryNotFound},
		{name: "directory", entryID: 12, wantError: domain.ErrFileEntryNotFile},
		{name: "root directory", entryID: 11, wantError: domain.ErrFileEntryNotFile},
		{name: "revision conflict", revision: 2, wantError: domain.ErrEntryRevisionConflict},
		{name: "hidden ancestor before conflict", revision: 2, editPath: func(reader *fileEntryPathReaderStub) {
			reader.entries[12].Status = "TRASHED"
		}, wantError: domain.ErrFileEntryNotFound},
		{name: "hidden file", editPath: func(reader *fileEntryPathReaderStub) {
			reader.entries[13].Status = "PURGED"
		}, wantError: domain.ErrFileEntryNotFound},
		{name: "nil content", editContent: func(*domain.FileEntryContent) *domain.FileEntryContent { return nil }, wantError: domain.ErrInvalidFileEntryData, readCalls: 1},
		{name: "wrong owner", editContent: func(content *domain.FileEntryContent) *domain.FileEntryContent {
			content.UserID++
			return content
		}, wantError: domain.ErrInvalidFileEntryData, readCalls: 1},
		{name: "wrong entry", editContent: func(content *domain.FileEntryContent) *domain.FileEntryContent {
			content.ID++
			return content
		}, wantError: domain.ErrInvalidFileEntryData, readCalls: 1},
		{name: "missing object reference", editContent: func(content *domain.FileEntryContent) *domain.FileEntryContent {
			content.ObjectID = nil
			return content
		}, wantError: domain.ErrDownloadTargetUnavailable, readCalls: 1},
		{name: "zero object ID", editContent: func(content *domain.FileEntryContent) *domain.FileEntryContent {
			content.ObjectID = new(uint64(0))
			return content
		}, wantError: domain.ErrInvalidFileEntryData, readCalls: 1},
		{name: "read failure", readError: readFailure, wantError: readFailure, readCalls: 1},
		{name: "cancelled snapshot read", readError: readFailure, wantError: context.Canceled, cancelInRead: true, readCalls: 1},
		{name: "snapshot close failure", commitError: snapshotFailure, wantError: snapshotFailure, readCalls: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 为每个场景构造一条完整路径和可控的内容引用读取。
			actor, pathReader := fileEntryPathFixture()
			entryID, revision := scenario.entryID, scenario.revision
			if entryID == 0 {
				entryID = 13
			}
			if revision == 0 {
				revision = 3
			}
			if scenario.editPath != nil {
				scenario.editPath(pathReader)
			}
			content := &domain.FileEntryContent{ID: entryID, UserID: actor.UserID, ObjectID: new(uint64(math.MaxUint64)), MIMEType: new("text/markdown")}
			if scenario.editContent != nil {
				content = scenario.editContent(content)
			}
			snapshotContext, cancel := context.WithCancel(t.Context())
			defer cancel()
			snapshotCalls, readCalls := 0, 0
			reader := &entryDownloadTargetReaderStub{fileEntryPathReaderStub: pathReader, readContent: func(ctx context.Context, userID, targetID uint64) (*domain.FileEntryContent, error) {
				readCalls++
				if ctx != snapshotContext || userID != actor.UserID || targetID != entryID || !reflect.DeepEqual(pathReader.entryIDs, []uint64{13, 12, 11}) {
					t.Error("内容引用未在完整路径检查后使用同一快照和归属读取")
				}
				if scenario.cancelInRead {
					cancel()
				}
				return content, scenario.readError
			}}
			targets := NewEntryDownloadTargetService(func(ctx context.Context, action func(context.Context, EntryDownloadTargetReader) error) error {
				snapshotCalls++
				if ctx != t.Context() {
					t.Error("快照入口未取得当前请求上下文")
				}
				if err := action(snapshotContext, reader); err != nil {
					return err
				}
				return scenario.commitError
			})

			// 2. 核对错误类别、读取顺序及独立的成功结果。
			target, err := targets.Get(t.Context(), actor, entryID, revision)
			if snapshotCalls != 1 || readCalls != scenario.readCalls || !errors.Is(err, scenario.wantError) {
				t.Fatal("下载目标调用次数或错误不符", snapshotCalls, readCalls, err)
			}
			if scenario.wantError != nil {
				if target != nil {
					t.Fatal("失败时返回了下载目标", target)
				}
				if errors.Is(err, domain.ErrEntryRevisionConflict) {
					var conflict *domain.EntryRevisionConflictError
					if !errors.As(err, &conflict) || conflict.CurrentRevision != 3 {
						t.Fatal("版本冲突缺少当前版本", err)
					}
				}
				return
			}
			want := domain.EntryDownloadTarget{
				EntryID: 13, UserID: actor.UserID, Name: "学习笔记.md", Revision: revision,
				ObjectID: math.MaxUint64, SizeBytes: 2048, MIMEType: content.MIMEType,
			}
			if !reflect.DeepEqual(target, &want) || (content.MIMEType != nil && target.MIMEType == content.MIMEType) {
				t.Fatal("下载目标内容不符或共享了 MIME 指针", target)
			}
		})
	}
}

func TestEntryDownloadTargetServiceRejectsInvalidInput(t *testing.T) {
	actor, _ := fileEntryPathFixture()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	targets := NewEntryDownloadTargetService(func(context.Context, func(context.Context, EntryDownloadTargetReader) error) error {
		t.Fatal("无效或取消请求开启了快照")
		return nil
	})
	for _, scenario := range []struct {
		ctx              context.Context
		identity         domain.Actor
		entryID, version uint64
	}{
		{nil, actor, 13, 3},
		{t.Context(), domain.Actor{}, 13, 3},
		{t.Context(), actor, 0, 3},
		{t.Context(), actor, 13, 0},
		{cancelled, actor, 13, 3},
	} {
		if target, err := targets.Get(scenario.ctx, scenario.identity, scenario.entryID, scenario.version); target != nil || err == nil ||
			(scenario.ctx == cancelled && !errors.Is(err, context.Canceled)) {
			t.Fatal("没有拒绝无效下载目标输入", target, err)
		}
	}
}
