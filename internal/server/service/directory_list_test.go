package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type directoryListReaderStub struct {
	*fileEntryPathReaderStub
	page             *domain.FileEntryPage
	listError        error
	listCalls        int
	listContext      context.Context
	userID, parentID uint64
	before           *domain.FileEntryPosition
	pageSize         int
	afterList        func()
	mutatePosition   bool
}

func (reader *directoryListReaderStub) ListUserDirectoryEntries(ctx context.Context, userID, parentID uint64, before *domain.FileEntryPosition, pageSize int) (*domain.FileEntryPage, error) {
	reader.listCalls++
	reader.listContext, reader.userID, reader.parentID, reader.pageSize = ctx, userID, parentID, pageSize
	if before != nil {
		copied := *before
		reader.before = &copied
		if reader.mutatePosition {
			before.ModifiedAt = before.ModifiedAt.Add(24 * time.Hour)
			before.ID = ^uint64(0)
		}
	}
	if reader.afterList != nil {
		reader.afterList()
	}
	return reader.page, reader.listError
}

func directoryListFixture() (domain.Actor, *directoryListReaderStub) {
	actor, pathReader := fileEntryPathFixture()
	file := *pathReader.entries[13]
	directory := file
	directory.ID, directory.Name, directory.EntryType, directory.FileSize = 14, "子目录", "DIR", 0
	directory.ModifiedAt = file.ModifiedAt.Add(-time.Second)
	return actor, &directoryListReaderStub{fileEntryPathReaderStub: pathReader, page: &domain.FileEntryPage{Entries: []domain.FileEntry{file, directory}, HasMore: true}}
}

func TestDirectoryListServiceResults(t *testing.T) {
	for _, mode := range []string{"first page", "next page", "empty page", "last page", "root directory", "maximum page size"} {
		t.Run(mode, func(t *testing.T) {
			actor, reader := directoryListFixture()
			parentID, pageSize := uint64(12), 2
			var before *domain.FileEntryPosition
			switch mode {
			case "next page":
				before = &domain.FileEntryPosition{ModifiedAt: reader.page.Entries[0].ModifiedAt.Add(time.Second).In(time.FixedZone("fixture", 8*3600)), ID: 99}
			case "empty page":
				reader.page.Entries, reader.page.HasMore = nil, false
			case "last page":
				reader.page.Entries, reader.page.HasMore = reader.page.Entries[:1], false
			case "root directory":
				parentID = 11
				reader.page = &domain.FileEntryPage{Entries: []domain.FileEntry{*reader.entries[12]}}
			case "maximum page size":
				pageSize, reader.page.HasMore = 100, false
			}
			requestContext := context.Background()
			type snapshotKey struct{}
			snapshotContext := context.WithValue(requestContext, snapshotKey{}, "snapshot")
			snapshots := 0
			lists := NewDirectoryListService(func(ctx context.Context, action func(context.Context, DirectoryListReader) error) error {
				snapshots++
				if ctx != requestContext {
					t.Error("快照入口上下文错误")
				}
				return action(snapshotContext, reader)
			})
			result, err := lists.List(requestContext, actor, parentID, before, pageSize)
			if err != nil || result == nil || snapshots != 1 || reader.listCalls != 1 || reader.listContext != snapshotContext || reader.userID != actor.UserID || reader.parentID != parentID || reader.pageSize != pageSize {
				t.Fatal("目录列表调用参数或单次快照错误", err)
			}
			for index, ctx := range reader.contexts {
				if ctx != snapshotContext || reader.userIDs[index] != actor.UserID {
					t.Fatal("路径查询未使用当前快照和用户归属")
				}
			}
			if result.Entries == nil || len(result.Entries) != len(reader.page.Entries) || result.HasMore != reader.page.HasMore || result.Path.Entries[len(result.Path.Entries)-1].ID != parentID {
				t.Fatal("路径、页或空数组错误")
			}
			for index, entry := range reader.page.Entries {
				if !reflect.DeepEqual(result.Entries[index], entry) {
					t.Fatal("子项基础字段丢失")
				}
			}
			if before != nil && (reader.before == before || !reader.before.ModifiedAt.Equal(before.ModifiedAt) || reader.before.ModifiedAt.Location() != time.UTC || before.ModifiedAt.Location() == time.UTC) {
				t.Fatal("翻页位置复制或 UTC 规范化错误")
			}
		})
	}
}

func TestDirectoryListServiceFailures(t *testing.T) {
	failure := errors.New("fixture list query")
	for _, testCase := range []struct {
		name     string
		parentID uint64
		change   func(*directoryListReaderStub)
		want     error
		calls    int
	}{
		{name: "missing parent", change: func(reader *directoryListReaderStub) { delete(reader.entries, 12) }, want: domain.ErrFileEntryNotFound},
		{name: "hidden parent", change: func(reader *directoryListReaderStub) { reader.entries[12].Status = "TRASHED" }, want: domain.ErrFileEntryNotFound},
		{name: "hidden ancestor", change: func(reader *directoryListReaderStub) { reader.entries[11].Status = "PURGING" }, want: domain.ErrFileEntryNotFound},
		{name: "file parent", parentID: 13, want: domain.ErrFileEntryNotDirectory},
		{name: "disabled account", change: func(reader *directoryListReaderStub) { reader.profile.Status = "DISABLED" }, want: domain.ErrAccountDisabled},
		{name: "bad parent path", change: func(reader *directoryListReaderStub) { reader.entries[12].UserID = 8 }, want: domain.ErrInvalidFileEntryData},
		{name: "list failure", change: func(reader *directoryListReaderStub) { reader.listError = failure }, want: failure, calls: 1},
		{name: "nil page", change: func(reader *directoryListReaderStub) { reader.page = nil }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "oversize page", change: func(reader *directoryListReaderStub) {
			reader.page.Entries = append(reader.page.Entries, reader.page.Entries[1])
		}, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "empty has more", change: func(reader *directoryListReaderStub) { reader.page.Entries = nil }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "short page has more", change: func(reader *directoryListReaderStub) { reader.page.Entries = reader.page.Entries[:1] }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "wrong owner", change: func(reader *directoryListReaderStub) { reader.page.Entries[0].UserID = 8 }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "wrong parent", change: func(reader *directoryListReaderStub) { parent := uint64(99); reader.page.Entries[0].ParentID = &parent }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "null parent", change: func(reader *directoryListReaderStub) { reader.page.Entries[0].ParentID = nil }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "inactive child", change: func(reader *directoryListReaderStub) { reader.page.Entries[0].Status = "TRASHED" }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "zero id", change: func(reader *directoryListReaderStub) { reader.page.Entries[0].ID = 0 }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "zero revision", change: func(reader *directoryListReaderStub) { reader.page.Entries[0].Revision = 0 }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "zero modified time", change: func(reader *directoryListReaderStub) { reader.page.Entries[0].ModifiedAt = time.Time{} }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "submicrosecond time", change: func(reader *directoryListReaderStub) {
			reader.page.Entries[0].ModifiedAt = reader.page.Entries[0].ModifiedAt.Add(time.Nanosecond)
		}, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "empty name", change: func(reader *directoryListReaderStub) { reader.page.Entries[0].Name = "" }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "unknown type", change: func(reader *directoryListReaderStub) { reader.page.Entries[0].EntryType = "UNKNOWN" }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "directory size", change: func(reader *directoryListReaderStub) { reader.page.Entries[1].FileSize = 1 }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "root returned as child", change: func(reader *directoryListReaderStub) { reader.page.Entries[0].ID = 11 }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "parent returned as child", change: func(reader *directoryListReaderStub) { reader.page.Entries[0].ID = 12 }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "duplicate id across times", change: func(reader *directoryListReaderStub) { reader.page.Entries[1].ID = reader.page.Entries[0].ID }, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "ascending time", change: func(reader *directoryListReaderStub) {
			reader.page.Entries[1].ModifiedAt = reader.page.Entries[0].ModifiedAt.Add(time.Second)
		}, want: domain.ErrInvalidFileEntryData, calls: 1},
		{name: "ascending tie ids", change: func(reader *directoryListReaderStub) {
			reader.page.Entries[1].ModifiedAt = reader.page.Entries[0].ModifiedAt
		}, want: domain.ErrInvalidFileEntryData, calls: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, reader := directoryListFixture()
			parentID := testCase.parentID
			if parentID == 0 {
				parentID = 12
			}
			if testCase.change != nil {
				testCase.change(reader)
			}
			lists := NewDirectoryListService(func(ctx context.Context, action func(context.Context, DirectoryListReader) error) error {
				return action(ctx, reader)
			})
			result, err := lists.List(context.Background(), actor, parentID, nil, 2)
			if result != nil || !errors.Is(err, testCase.want) || reader.listCalls != testCase.calls {
				t.Fatal("列表错误分类或路径检查顺序错误", err, reader.listCalls)
			}
		})
	}
}

func TestDirectoryListServiceBoundsAndCopies(t *testing.T) {
	t.Run("bound survives dependency mutation", func(t *testing.T) {
		actor, reader := directoryListFixture()
		before := &domain.FileEntryPosition{ModifiedAt: reader.page.Entries[0].ModifiedAt.Add(-time.Second), ID: 99}
		original := *before
		reader.mutatePosition = true
		lists := NewDirectoryListService(func(ctx context.Context, action func(context.Context, DirectoryListReader) error) error {
			return action(ctx, reader)
		})
		if result, err := lists.List(context.Background(), actor, 12, before, 2); result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) || !reflect.DeepEqual(*before, original) {
			t.Fatal("翻页界限被依赖或调用修改", err)
		}
	})
	t.Run("independent path and entries", func(t *testing.T) {
		actor, reader := directoryListFixture()
		lists := NewDirectoryListService(func(ctx context.Context, action func(context.Context, DirectoryListReader) error) error {
			return action(ctx, reader)
		})
		first, err := lists.List(context.Background(), actor, 12, nil, 2)
		if err != nil {
			t.Fatal(err)
		}
		second, err := lists.List(context.Background(), actor, 12, nil, 2)
		if err != nil || second == nil {
			t.Fatal(err)
		}
		reader.entries[12].Name = "来源路径修改"
		reader.page.Entries[0].Name = "来源子项修改"
		*reader.page.Entries[0].ParentID = 99
		if first.Path.Entries[1].Name != "课程资料 🌱" || first.Entries[0].Name != "学习笔记.md" || *first.Entries[0].ParentID != 12 {
			t.Fatal("来源修改影响结果")
		}
		first.Path.Entries[1].Name = "本地路径修改"
		first.Entries[0].Name = "本地子项修改"
		*first.Entries[0].ParentID = 88
		if second.Path.Entries[1].Name != "课程资料 🌱" || second.Entries[0].Name != "学习笔记.md" || *second.Entries[0].ParentID != 12 {
			t.Fatal("重复结果共享可变数据")
		}
	})
	for _, testCase := range []struct {
		depth     int
		child     bool
		wantError bool
	}{{31, true, false}, {32, false, false}, {32, true, true}} {
		t.Run(fmt.Sprintf("depth %d child %v", testCase.depth, testCase.child), func(t *testing.T) {
			actor, reader := directoryListFixture()
			parentID := uint64(11)
			for depth := 1; depth <= testCase.depth; depth++ {
				entry := *reader.entries[11]
				parent := parentID
				entry.ID, entry.ParentID, entry.Name = uint64(100+depth), &parent, fmt.Sprint(depth)
				reader.entries[entry.ID] = &entry
				parentID = entry.ID
			}
			reader.page = &domain.FileEntryPage{}
			if testCase.child {
				child := *reader.entries[13]
				child.ID, child.ParentID = 1000, &parentID
				reader.page.Entries = []domain.FileEntry{child}
			}
			lists := NewDirectoryListService(func(ctx context.Context, action func(context.Context, DirectoryListReader) error) error {
				return action(ctx, reader)
			})
			result, err := lists.List(context.Background(), actor, parentID, nil, 2)
			if testCase.wantError {
				if result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
					t.Fatal("超深子项未拒绝", err)
				}
			} else if err != nil || result == nil || len(result.Path.Entries) != testCase.depth+1 {
				t.Fatal("合法深度列表失败", err)
			}
		})
	}
}

func TestDirectoryListServiceInputAndSnapshot(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero session", "zero device", "zero parent", "zero size", "oversize", "invalid position", "cancelled", "after path", "after list", "cancel with failure", "deadline", "end failure", "cancel after snapshot", "missing callback", "nil snapshot context", "nil reader"} {
		t.Run(mode, func(t *testing.T) {
			actor, reader := directoryListFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			parentID, pageSize := uint64(12), 2
			var before *domain.FileEntryPosition
			switch mode {
			case "nil context":
				ctx = nil
			case "zero user":
				actor.UserID = 0
			case "zero session":
				actor.SessionID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero parent":
				parentID = 0
			case "zero size":
				pageSize = 0
			case "oversize":
				pageSize = 101
			case "invalid position":
				before = &domain.FileEntryPosition{ID: 99}
			case "cancelled":
				cancel()
			case "after path":
				reader.afterEntry = func(uint64) { cancel() }
			case "after list":
				reader.afterList = cancel
			case "cancel with failure":
				reader.afterList = cancel
				reader.listError = errors.New("fixture query")
			case "deadline":
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 20*time.Millisecond)
				defer release()
				reader.afterList = func() { <-ctx.Done() }
			}
			snapshots := 0
			failure := errors.New("fixture end")
			lists := NewDirectoryListService(func(ctx context.Context, action func(context.Context, DirectoryListReader) error) error {
				snapshots++
				switch mode {
				case "missing callback":
					return nil
				case "nil snapshot context":
					return action(nil, reader)
				case "nil reader":
					return action(ctx, nil)
				}
				if err := action(ctx, reader); err != nil {
					return err
				}
				if mode == "end failure" {
					return failure
				}
				if mode == "cancel after snapshot" {
					cancel()
				}
				return nil
			})
			result, err := lists.List(ctx, actor, parentID, before, pageSize)
			if result != nil || err == nil {
				t.Fatal("无效输入或快照异常仍返回列表")
			}
			if mode == "cancelled" || mode == "after path" || mode == "after list" || mode == "cancel with failure" || mode == "cancel after snapshot" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("取消原因丢失", err)
				}
			} else if mode == "deadline" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("期限原因丢失", err)
				}
			} else if mode == "end failure" {
				if !errors.Is(err, failure) {
					t.Fatal("快照结束原因丢失", err)
				}
			}
			if mode == "nil context" || mode == "zero user" || mode == "zero session" || mode == "zero device" || mode == "zero parent" || mode == "zero size" || mode == "oversize" || mode == "invalid position" || mode == "cancelled" {
				if snapshots != 0 {
					t.Fatal("非法输入仍进入快照")
				}
			}
		})
	}
}

func TestDirectoryListServiceDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少快照依赖仍构造成功")
		}
	}()
	NewDirectoryListService(nil)
}
