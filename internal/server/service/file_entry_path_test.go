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

type fileEntryPathReaderStub struct {
	profile      *domain.UserProfile
	profileError error
	entries      map[uint64]*domain.FileEntry
	entryErrors  map[uint64]error
	entryIDs     []uint64
	userIDs      []uint64
	contexts     []context.Context
	afterProfile func()
	afterEntry   func(uint64)
}

func (reader *fileEntryPathReaderStub) FindUserProfile(ctx context.Context, userID uint64) (*domain.UserProfile, error) {
	reader.contexts = append(reader.contexts, ctx)
	reader.userIDs = append(reader.userIDs, userID)
	if reader.afterProfile != nil {
		reader.afterProfile()
	}
	return reader.profile, reader.profileError
}

func (reader *fileEntryPathReaderStub) FindUserFileEntry(ctx context.Context, userID, entryID uint64) (*domain.FileEntry, error) {
	reader.contexts = append(reader.contexts, ctx)
	reader.userIDs = append(reader.userIDs, userID)
	reader.entryIDs = append(reader.entryIDs, entryID)
	if reader.afterEntry != nil {
		reader.afterEntry(entryID)
	}
	if err := reader.entryErrors[entryID]; err != nil {
		return nil, err
	}
	entry, exists := reader.entries[entryID]
	if !exists {
		return nil, domain.ErrFileEntryNotFound
	}
	return entry, nil
}

func fileEntryPathFixture() (domain.Actor, *fileEntryPathReaderStub) {
	rootID, directoryID := uint64(11), uint64(12)
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	root := &domain.FileEntry{ID: rootID, UserID: 7, EntryType: "DIR", Revision: 1, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: stamp, UpdatedAt: stamp}
	directory, file := *root, *root
	directory.ID, directory.ParentID, directory.Name = directoryID, &rootID, "课程资料 🌱"
	file.ID, file.ParentID, file.Name, file.EntryType, file.FileSize, file.Revision = 13, &directoryID, "学习笔记.md", "FILE", 2048, 3
	return domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}, &fileEntryPathReaderStub{
		profile: &domain.UserProfile{ID: 7, Status: "ACTIVE", RootEntryID: rootID, QuotaBytes: 100},
		entries: map[uint64]*domain.FileEntry{11: root, 12: &directory, 13: &file}, entryErrors: make(map[uint64]error),
	}
}

func TestFileEntryPathServiceResults(t *testing.T) {
	for _, targetID := range []uint64{11, 12, 13} {
		t.Run(fmt.Sprint(targetID), func(t *testing.T) {
			actor, reader := fileEntryPathFixture()
			requestContext := context.Background()
			type contextKey struct{}
			snapshotContext := context.WithValue(requestContext, contextKey{}, "snapshot")
			calls := 0
			paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
				calls++
				if ctx != requestContext {
					t.Error("快照入口收到的上下文错误")
				}
				return action(snapshotContext, reader)
			})
			result, err := paths.Get(requestContext, actor, targetID)
			if err != nil || result == nil || calls != 1 || len(result.Entries) != int(targetID-10) {
				t.Fatal("根目录、目录或文件路径错误", err)
			}
			for index, entry := range result.Entries {
				if entry.ID != uint64(index+11) || !reflect.DeepEqual(entry, *reader.entries[entry.ID]) {
					t.Fatal("路径顺序或基础字段错误")
				}
			}
			for index, ctx := range reader.contexts {
				if ctx != snapshotContext || reader.userIDs[index] != actor.UserID {
					t.Fatal("资料或目录项读取未使用同一快照及用户归属")
				}
			}
			for index, entryID := range reader.entryIDs {
				if entryID != targetID-uint64(index) {
					t.Fatal("父链未从目标向根目录读取")
				}
			}
		})
	}
}

func TestFileEntryPathServiceFailures(t *testing.T) {
	failure := errors.New("fixture query")
	for _, testCase := range []struct {
		name   string
		change func(*fileEntryPathReaderStub)
		want   error
	}{
		{name: "missing target", change: func(reader *fileEntryPathReaderStub) { delete(reader.entries, 13) }, want: domain.ErrFileEntryNotFound},
		{name: "missing ancestor", change: func(reader *fileEntryPathReaderStub) { delete(reader.entries, 12) }, want: domain.ErrInvalidFileEntryData},
		{name: "nil record", change: func(reader *fileEntryPathReaderStub) { reader.entries[13] = nil }, want: domain.ErrInvalidFileEntryData},
		{name: "wrong target", change: func(reader *fileEntryPathReaderStub) { reader.entries[13].ID = 99 }, want: domain.ErrInvalidFileEntryData},
		{name: "wrong owner", change: func(reader *fileEntryPathReaderStub) { reader.entries[12].UserID = 8 }, want: domain.ErrInvalidFileEntryData},
		{name: "zero revision", change: func(reader *fileEntryPathReaderStub) { reader.entries[13].Revision = 0 }, want: domain.ErrInvalidFileEntryData},
		{name: "zero parent", change: func(reader *fileEntryPathReaderStub) { zero := uint64(0); reader.entries[13].ParentID = &zero }, want: domain.ErrInvalidFileEntryData},
		{name: "file ancestor", change: func(reader *fileEntryPathReaderStub) { reader.entries[12].EntryType = "FILE" }, want: domain.ErrInvalidFileEntryData},
		{name: "unknown type", change: func(reader *fileEntryPathReaderStub) { reader.entries[13].EntryType = "UNKNOWN" }, want: domain.ErrInvalidFileEntryData},
		{name: "unknown status", change: func(reader *fileEntryPathReaderStub) { reader.entries[13].Status = "UNKNOWN" }, want: domain.ErrInvalidFileEntryData},
		{name: "directory size", change: func(reader *fileEntryPathReaderStub) { reader.entries[12].FileSize = 1 }, want: domain.ErrInvalidFileEntryData},
		{name: "nonroot empty name", change: func(reader *fileEntryPathReaderStub) { reader.entries[13].Name = "" }, want: domain.ErrInvalidFileEntryData},
		{name: "other null parent", change: func(reader *fileEntryPathReaderStub) { reader.entries[12].ParentID = nil }, want: domain.ErrInvalidFileEntryData},
		{name: "named root", change: func(reader *fileEntryPathReaderStub) { reader.entries[11].Name = "假根目录" }, want: domain.ErrInvalidFileEntryData},
		{name: "root has parent", change: func(reader *fileEntryPathReaderStub) { parent := uint64(12); reader.entries[11].ParentID = &parent }, want: domain.ErrInvalidFileEntryData},
		{name: "self cycle", change: func(reader *fileEntryPathReaderStub) { parent := uint64(12); reader.entries[12].ParentID = &parent }, want: domain.ErrInvalidFileEntryData},
		{name: "two node cycle", change: func(reader *fileEntryPathReaderStub) { parent := uint64(13); reader.entries[12].ParentID = &parent }, want: domain.ErrInvalidFileEntryData},
		{name: "disabled account", change: func(reader *fileEntryPathReaderStub) { reader.profile.Status = "DISABLED" }, want: domain.ErrAccountDisabled},
		{name: "missing account", change: func(reader *fileEntryPathReaderStub) { reader.profileError = domain.ErrAccountNotFound }, want: domain.ErrAccountNotFound},
		{name: "nil profile", change: func(reader *fileEntryPathReaderStub) { reader.profile = nil }, want: domain.ErrInvalidAccountData},
		{name: "wrong profile owner", change: func(reader *fileEntryPathReaderStub) { reader.profile.ID = 8 }, want: domain.ErrInvalidAccountData},
		{name: "missing root id", change: func(reader *fileEntryPathReaderStub) { reader.profile.RootEntryID = 0 }, want: domain.ErrInvalidAccountData},
		{name: "unknown account state", change: func(reader *fileEntryPathReaderStub) { reader.profile.Status = "UNKNOWN" }, want: domain.ErrInvalidAccountData},
		{name: "query failure", change: func(reader *fileEntryPathReaderStub) { reader.entryErrors[12] = failure }, want: failure},
		{name: "profile failure", change: func(reader *fileEntryPathReaderStub) { reader.profileError = failure }, want: failure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, reader := fileEntryPathFixture()
			testCase.change(reader)
			paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
				return action(ctx, reader)
			})
			result, err := paths.Get(context.Background(), actor, 13)
			if result != nil || !errors.Is(err, testCase.want) || len(reader.entryIDs) > domain.MaxFileEntryDepth+1 {
				t.Fatal("路径错误分类或查询上限错误", err)
			}
		})
	}
	for _, targetID := range []uint64{12, 13} {
		for _, status := range []string{"TRASHED", "PURGING", "PURGED"} {
			t.Run(fmt.Sprintf("hidden %d %s", targetID, status), func(t *testing.T) {
				actor, reader := fileEntryPathFixture()
				reader.entries[targetID].Status = status
				paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
					return action(ctx, reader)
				})
				if result, err := paths.Get(context.Background(), actor, 13); result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
					t.Fatal("回收或清理中的目标/祖先仍可见", err)
				}
			})
		}
	}
}

func TestFileEntryPathServiceDepthAndCopies(t *testing.T) {
	for _, depth := range []int{32, 33} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			actor, reader := fileEntryPathFixture()
			parentID := uint64(11)
			for level := 1; level <= depth; level++ {
				entry := *reader.entries[11]
				parent := parentID
				entry.ID, entry.ParentID, entry.Name = uint64(100+level), &parent, fmt.Sprintf("level-%d", level)
				reader.entries[entry.ID] = &entry
				parentID = entry.ID
			}
			paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
				return action(ctx, reader)
			})
			result, err := paths.Get(context.Background(), actor, parentID)
			if depth == 32 {
				if err != nil || result == nil || len(result.Entries) != 33 {
					t.Fatal("第 32 层路径被拒绝", err)
				}
			} else if result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) || len(reader.entryIDs) != 33 {
				t.Fatal("超过 32 层未按上限拒绝", err)
			}
		})
	}
	t.Run("maximum ids size and revision", func(t *testing.T) {
		actor, reader := fileEntryPathFixture()
		maximum, parent := ^uint64(0), ^uint64(0)-1
		root, target := *reader.entries[11], *reader.entries[13]
		root.ID, root.UserID = parent, maximum
		target.ID, target.UserID, target.ParentID, target.FileSize, target.Revision = maximum, maximum, &parent, maximum, maximum
		actor.UserID, reader.profile.ID, reader.profile.RootEntryID = maximum, maximum, parent
		reader.entries = map[uint64]*domain.FileEntry{parent: &root, maximum: &target}
		paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
			return action(ctx, reader)
		})
		result, err := paths.Get(context.Background(), actor, maximum)
		if err != nil || result == nil || len(result.Entries) != 2 || !reflect.DeepEqual(result.Entries[1], target) {
			t.Fatal("完整 uint64 路径字段错误", err)
		}
	})
	t.Run("source and repeated results are independent", func(t *testing.T) {
		actor, reader := fileEntryPathFixture()
		paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
			return action(ctx, reader)
		})
		first, err := paths.Get(context.Background(), actor, 13)
		if err != nil {
			t.Fatal(err)
		}
		second, err := paths.Get(context.Background(), actor, 13)
		if err != nil || second == nil {
			t.Fatal(err)
		}
		reader.entries[13].Name, *reader.entries[13].ParentID = "来源修改", 99
		if first.Entries[2].Name != "学习笔记.md" || *first.Entries[2].ParentID != 12 {
			t.Fatal("来源变化影响已复制路径")
		}
		first.Entries[2].Name, *first.Entries[2].ParentID = "结果修改", 88
		if second.Entries[2].Name != "学习笔记.md" || *second.Entries[2].ParentID != 12 {
			t.Fatal("重复结果共享路径或父目录指针")
		}
	})
}

func TestFileEntryPathServiceSnapshotBoundary(t *testing.T) {
	failure := errors.New("fixture snapshot end")
	for _, mode := range []string{"missing callback", "nil context", "nil reader", "end failure", "cancel after read"} {
		t.Run(mode, func(t *testing.T) {
			actor, reader := fileEntryPathFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
				switch mode {
				case "missing callback":
					return nil
				case "nil context":
					return action(nil, reader)
				case "nil reader":
					return action(ctx, nil)
				}
				if err := action(ctx, reader); err != nil {
					return err
				}
				if mode == "cancel after read" {
					cancel()
					return nil
				}
				return failure
			})
			result, err := paths.Get(ctx, actor, 13)
			if result != nil || err == nil || (mode == "end failure" && !errors.Is(err, failure)) || (mode == "cancel after read" && !errors.Is(err, context.Canceled)) {
				t.Fatal("快照异常后仍交付路径或丢失原因", err)
			}
		})
	}
}

func TestFileEntryPathServiceInputAndCancellation(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero session", "zero device", "zero target", "cancelled", "after profile", "after target", "cancel with missing ancestor", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			actor, reader := fileEntryPathFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entryID, calls := uint64(13), 0
			switch mode {
			case "nil context":
				ctx = nil
			case "zero user":
				actor.UserID = 0
			case "zero session":
				actor.SessionID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero target":
				entryID = 0
			case "cancelled":
				cancel()
			case "after profile":
				reader.afterProfile = cancel
			case "after target":
				reader.afterEntry = func(uint64) { cancel() }
			case "cancel with missing ancestor":
				delete(reader.entries, 12)
				reader.afterEntry = func(entryID uint64) {
					if entryID == 12 {
						cancel()
					}
				}
			case "deadline":
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 20*time.Millisecond)
				defer release()
				reader.afterProfile = func() { <-ctx.Done() }
			}
			paths := NewFileEntryPathService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
				calls++
				return action(ctx, reader)
			})
			result, err := paths.Get(ctx, actor, entryID)
			if result != nil || err == nil {
				t.Fatal("非法输入或取消返回成功路径")
			}
			if mode == "cancelled" || mode == "after profile" || mode == "after target" || mode == "cancel with missing ancestor" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("取消原因丢失", err)
				}
			} else if mode == "deadline" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal("期限原因丢失", err)
				}
			} else if calls != 0 {
				t.Fatal("非法输入访问了快照")
			}
		})
	}
}

func TestFileEntryPathServiceDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少快照依赖仍构造成功")
		}
	}()
	NewFileEntryPathService(nil)
}
