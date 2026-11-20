package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type entryDownloadTargetReaderStub struct {
	*fileEntryPathReaderStub
	readContent func(context.Context, uint64, uint64) (*domain.FileEntryContent, error)
	readObject  func(context.Context, uint64, uint64) (*domain.StoredObject, error)
}

func (reader *entryDownloadTargetReaderStub) FindUserFileEntryContent(ctx context.Context, userID, entryID uint64) (*domain.FileEntryContent, error) {
	return reader.readContent(ctx, userID, entryID)
}

func (reader *entryDownloadTargetReaderStub) FindUserStoredObject(ctx context.Context, userID, objectID uint64) (*domain.StoredObject, error) {
	return reader.readObject(ctx, userID, objectID)
}

func TestEntryDownloadTargetService(t *testing.T) {
	readFailure := errors.New("fixture content read failure")
	snapshotFailure := errors.New("fixture snapshot close failure")
	for _, scenario := range []struct {
		name                   string
		entryID, revision      uint64
		editPath               func(*fileEntryPathReaderStub)
		editContent            func(*domain.FileEntryContent) *domain.FileEntryContent
		editObject             func(*domain.StoredObject) *domain.StoredObject
		readError, commitError error
		objectError            error
		wantError              error
		readCalls, objectCalls int
		cancelInRead           bool
		cancelInObjectRead     bool
	}{
		{name: "visible file", readCalls: 1, objectCalls: 1},
		{name: "unknown MIME", editContent: func(content *domain.FileEntryContent) *domain.FileEntryContent {
			content.MIMEType = nil
			return content
		}, readCalls: 1, objectCalls: 1},
		{name: "full unsigned revision", revision: math.MaxUint64, editPath: func(reader *fileEntryPathReaderStub) {
			reader.entries[13].Revision = math.MaxUint64
		}, readCalls: 1, objectCalls: 1},
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
		{name: "snapshot close failure", commitError: snapshotFailure, wantError: snapshotFailure, readCalls: 1, objectCalls: 1},
		{name: "missing stored object", objectError: domain.ErrStoredObjectNotFound, wantError: domain.ErrDownloadTargetUnavailable, readCalls: 1, objectCalls: 1},
		{name: "stored object read failure", objectError: readFailure, wantError: readFailure, readCalls: 1, objectCalls: 1},
		{name: "cancelled object read", objectError: domain.ErrStoredObjectNotFound, wantError: context.Canceled, cancelInObjectRead: true, readCalls: 1, objectCalls: 1},
		{name: "nil stored object", editObject: func(*domain.StoredObject) *domain.StoredObject {
			return nil
		}, wantError: domain.ErrInvalidStoredObjectData, readCalls: 1, objectCalls: 1},
		{name: "other stored object ID", editObject: func(object *domain.StoredObject) *domain.StoredObject {
			object.ID--
			return object
		}, wantError: domain.ErrInvalidStoredObjectData, readCalls: 1, objectCalls: 1},
		{name: "other stored object owner", editObject: func(object *domain.StoredObject) *domain.StoredObject {
			object.OwnerUserID++
			return object
		}, wantError: domain.ErrInvalidStoredObjectData, readCalls: 1, objectCalls: 1},
		{name: "unknown stored object status", editObject: func(object *domain.StoredObject) *domain.StoredObject {
			object.Status = "UNKNOWN"
			return object
		}, wantError: domain.ErrInvalidStoredObjectData, readCalls: 1, objectCalls: 1},
		{name: "stored object still verifying", editObject: func(object *domain.StoredObject) *domain.StoredObject {
			object.Status = domain.StoredObjectStatusVerifying
			return object
		}, wantError: domain.ErrDownloadTargetUnavailable, readCalls: 1, objectCalls: 1},
		{name: "inconsistent stored size", editObject: func(object *domain.StoredObject) *domain.StoredObject {
			object.FileSize++
			return object
		}, wantError: domain.ErrInvalidStoredObjectData, readCalls: 1, objectCalls: 1},
		{name: "missing verified digest", editObject: func(object *domain.StoredObject) *domain.StoredObject {
			object.VerifiedSHA256 = nil
			return object
		}, wantError: domain.ErrInvalidStoredObjectData, readCalls: 1, objectCalls: 1},
		{name: "missing verification time", editObject: func(object *domain.StoredObject) *domain.StoredObject {
			object.VerifiedAt = nil
			return object
		}, wantError: domain.ErrInvalidStoredObjectData, readCalls: 1, objectCalls: 1},
		{name: "missing stored key", editObject: func(object *domain.StoredObject) *domain.StoredObject {
			object.ObjectKey = ""
			return object
		}, wantError: domain.ErrInvalidStoredObjectData, readCalls: 1, objectCalls: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备路径、内容引用和就绪对象，按场景替换其中的读回事实。
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
			digest := sha256.Sum256([]byte("fixture download content"))
			object := &domain.StoredObject{
				ID: math.MaxUint64, OwnerUserID: actor.UserID, FileSize: 2048, Status: domain.StoredObjectStatusReady,
				Provider: "local_minio", Bucket: "fixture-bucket", ObjectKey: "objects/fixture", StorageVersionID: new("fixture-version"),
				VerifiedSHA256: &digest, VerifiedAt: new(time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)),
			}
			if scenario.editObject != nil {
				object = scenario.editObject(object)
			}
			snapshotContext, cancel := context.WithCancel(t.Context())
			defer cancel()
			snapshotCalls, readCalls, objectCalls := 0, 0, 0
			reader := &entryDownloadTargetReaderStub{fileEntryPathReaderStub: pathReader, readContent: func(ctx context.Context, userID, targetID uint64) (*domain.FileEntryContent, error) {
				readCalls++
				if ctx != snapshotContext || userID != actor.UserID || targetID != entryID || !reflect.DeepEqual(pathReader.entryIDs, []uint64{13, 12, 11}) {
					t.Error("内容引用未在完整路径检查后使用同一快照和归属读取")
				}
				if scenario.cancelInRead {
					cancel()
				}
				return content, scenario.readError
			}, readObject: func(ctx context.Context, userID, objectID uint64) (*domain.StoredObject, error) {
				objectCalls++
				if ctx != snapshotContext || userID != actor.UserID || objectID != math.MaxUint64 || readCalls != 1 {
					t.Error("存储对象未使用同一快照及内容引用中的归属和对象 ID")
				}
				if scenario.cancelInObjectRead {
					cancel()
				}
				return object, scenario.objectError
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
			if snapshotCalls != 1 || readCalls != scenario.readCalls || objectCalls != scenario.objectCalls || !errors.Is(err, scenario.wantError) {
				t.Fatal("下载目标调用次数或错误不符", snapshotCalls, readCalls, objectCalls, err)
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
				Provider: "local_minio", Bucket: "fixture-bucket", ObjectKey: "objects/fixture",
				StorageVersionID: new("fixture-version"), VerifiedSHA256: digest,
			}
			if !reflect.DeepEqual(target, &want) || (content.MIMEType != nil && target.MIMEType == content.MIMEType) ||
				target.StorageVersionID == object.StorageVersionID {
				t.Fatal("下载目标内容不符或共享了可空字段指针", target)
			}
			object.VerifiedSHA256[0] ^= 0xff
			if target.VerifiedSHA256 != want.VerifiedSHA256 {
				t.Fatal("下载目标未独立持有摘要", target)
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
