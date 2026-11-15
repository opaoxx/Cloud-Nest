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

type entryDetailReaderStub struct {
	*fileEntryPathReaderStub
	display                           *domain.FileEntryDisplay
	preference                        *domain.EntryPreference
	displayError, preferenceError     error
	displayCalls, preferenceCalls     int
	displayContext, preferenceContext context.Context
	displayUser, displayTarget        uint64
	preferenceUser, preferenceTarget  uint64
	afterDisplay, afterPreference     func()
}

func (reader *entryDetailReaderStub) FindUserFileEntryDisplay(ctx context.Context, userID, entryID uint64) (*domain.FileEntryDisplay, error) {
	reader.displayCalls++
	reader.displayContext, reader.displayUser, reader.displayTarget = ctx, userID, entryID
	if reader.afterDisplay != nil {
		reader.afterDisplay()
	}
	return reader.display, reader.displayError
}

func (reader *entryDetailReaderStub) FindUserEntryPreference(ctx context.Context, userID, entryID uint64) (*domain.EntryPreference, error) {
	reader.preferenceCalls++
	reader.preferenceContext, reader.preferenceUser, reader.preferenceTarget = ctx, userID, entryID
	if reader.afterPreference != nil {
		reader.afterPreference()
	}
	return reader.preference, reader.preferenceError
}

func entryDetailFixture() (domain.Actor, *entryDetailReaderStub) {
	actor, paths := fileEntryPathFixture()
	category, extension, mimeType := "DOCUMENT", "md", "text/markdown"
	sourceName, color := "发布时设备 🎮", "green"
	sourceID := uint64(19)
	contentModifiedAt := paths.entries[13].ModifiedAt
	starredAt, lastOpenedAt := contentModifiedAt, contentModifiedAt
	return actor, &entryDetailReaderStub{
		fileEntryPathReaderStub: paths,
		display: &domain.FileEntryDisplay{
			ID: 13, UserID: actor.UserID, Category: &category, Extension: &extension, MIMEType: &mimeType,
			SourceDeviceID: &sourceID, SourceDeviceName: &sourceName, ColorKey: &color, ContentModifiedAt: &contentModifiedAt,
		},
		preference: &domain.EntryPreference{UserID: actor.UserID, EntryID: 13, StarredAt: &starredAt, LastOpenedAt: &lastOpenedAt},
	}
}

func TestEntryDetailServiceResults(t *testing.T) {
	for _, mode := range []string{"file", "directory", "root", "nullable display", "default preference", "maximum unsigned values"} {
		t.Run(mode, func(t *testing.T) {
			actor, reader := entryDetailFixture()
			targetID := uint64(13)
			switch mode {
			case "directory", "root":
				targetID = 12
				if mode == "root" {
					targetID = 11
				}
				reader.display = &domain.FileEntryDisplay{ID: targetID, UserID: actor.UserID, ColorKey: reader.display.ColorKey}
				reader.preference.EntryID = targetID
			case "nullable display":
				empty := ""
				reader.display = &domain.FileEntryDisplay{ID: targetID, UserID: actor.UserID, Extension: &empty}
			case "default preference":
				reader.preference = &domain.EntryPreference{UserID: actor.UserID, EntryID: targetID}
			case "maximum unsigned values":
				maximum, parentID := ^uint64(0), ^uint64(0)-1
				root, target := *reader.entries[11], *reader.entries[13]
				root.ID, root.UserID = parentID, maximum
				target.ID, target.UserID, target.ParentID, target.FileSize, target.Revision = maximum, maximum, &parentID, maximum, maximum
				actor = domain.Actor{UserID: maximum, SessionID: maximum, DeviceID: maximum}
				reader.profile.ID, reader.profile.RootEntryID = maximum, parentID
				reader.entries = map[uint64]*domain.FileEntry{parentID: &root, maximum: &target}
				reader.display.ID, reader.display.UserID, reader.display.SourceDeviceID = maximum, maximum, &maximum
				reader.preference.UserID, reader.preference.EntryID = maximum, maximum
				targetID = maximum
			}
			type snapshotKey struct{}
			requestContext := context.Background()
			snapshotContext := context.WithValue(requestContext, snapshotKey{}, "snapshot")
			snapshots := 0
			details := NewEntryDetailService(func(ctx context.Context, action func(context.Context, EntryDetailReader) error) error {
				snapshots++
				if ctx != requestContext {
					t.Error("详情快照入口上下文错误")
				}
				return action(snapshotContext, reader)
			})
			result, err := details.Get(requestContext, actor, targetID)
			if err != nil || result == nil || snapshots != 1 || reader.displayCalls != 1 || reader.preferenceCalls != 1 ||
				reader.displayContext != snapshotContext || reader.preferenceContext != snapshotContext ||
				reader.displayUser != actor.UserID || reader.preferenceUser != actor.UserID || reader.displayTarget != targetID || reader.preferenceTarget != targetID {
				t.Fatal("详情单次快照、上下文、调用次数或归属错误", err)
			}
			if !reflect.DeepEqual(result.Display, *reader.display) || !reflect.DeepEqual(result.Preference, *reader.preference) ||
				!reflect.DeepEqual(result.Path.Entries[len(result.Path.Entries)-1], *reader.entries[targetID]) {
				t.Fatal("详情目标、展示字段或偏好丢失")
			}
			for index, entry := range result.Path.Entries {
				if !reflect.DeepEqual(entry, *reader.entries[entry.ID]) || reader.entryIDs[len(reader.entryIDs)-1-index] != entry.ID {
					t.Fatal("详情路径顺序或基础字段错误")
				}
			}
			for index, ctx := range reader.contexts {
				if ctx != snapshotContext || reader.userIDs[index] != actor.UserID {
					t.Fatal("路径读取未使用详情快照和准确用户")
				}
			}
		})
	}
}

func TestEntryDetailServiceFailures(t *testing.T) {
	failure := errors.New("fixture detail query")
	for _, testCase := range []struct {
		name                          string
		change                        func(*entryDetailReaderStub)
		want                          error
		displayCalls, preferenceCalls int
	}{
		{name: "missing target", change: func(reader *entryDetailReaderStub) { delete(reader.entries, 13) }, want: domain.ErrFileEntryNotFound},
		{name: "hidden target", change: func(reader *entryDetailReaderStub) { reader.entries[13].Status = "TRASHED" }, want: domain.ErrFileEntryNotFound},
		{name: "hidden ancestor", change: func(reader *entryDetailReaderStub) { reader.entries[12].Status = "PURGING" }, want: domain.ErrFileEntryNotFound},
		{name: "disabled account", change: func(reader *entryDetailReaderStub) { reader.profile.Status = "DISABLED" }, want: domain.ErrAccountDisabled},
		{name: "path failure", change: func(reader *entryDetailReaderStub) { reader.entryErrors[13] = failure }, want: failure},
		{name: "invalid parent chain", change: func(reader *entryDetailReaderStub) { reader.entries[12].EntryType = "FILE" }, want: domain.ErrInvalidFileEntryData},
		{name: "display failure", change: func(reader *entryDetailReaderStub) { reader.displayError = failure }, want: failure, displayCalls: 1},
		{name: "display missing", change: func(reader *entryDetailReaderStub) { reader.displayError = domain.ErrFileEntryNotFound }, want: domain.ErrFileEntryNotFound, displayCalls: 1},
		{name: "nil display", change: func(reader *entryDetailReaderStub) { reader.display = nil }, want: domain.ErrInvalidFileEntryData, displayCalls: 1},
		{name: "foreign display", change: func(reader *entryDetailReaderStub) { reader.display.UserID = 8 }, want: domain.ErrInvalidFileEntryData, displayCalls: 1},
		{name: "wrong display target", change: func(reader *entryDetailReaderStub) { reader.display.ID = 14 }, want: domain.ErrInvalidFileEntryData, displayCalls: 1},
		{name: "zero source device", change: func(reader *entryDetailReaderStub) { *reader.display.SourceDeviceID = 0 }, want: domain.ErrInvalidFileEntryData, displayCalls: 1},
		{name: "preference failure", change: func(reader *entryDetailReaderStub) { reader.preferenceError = failure }, want: failure, displayCalls: 1, preferenceCalls: 1},
		{name: "preference missing", change: func(reader *entryDetailReaderStub) { reader.preferenceError = domain.ErrFileEntryNotFound }, want: domain.ErrFileEntryNotFound, displayCalls: 1, preferenceCalls: 1},
		{name: "nil preference", change: func(reader *entryDetailReaderStub) { reader.preference = nil }, want: domain.ErrInvalidFileEntryData, displayCalls: 1, preferenceCalls: 1},
		{name: "foreign preference", change: func(reader *entryDetailReaderStub) { reader.preference.UserID = 8 }, want: domain.ErrInvalidFileEntryData, displayCalls: 1, preferenceCalls: 1},
		{name: "wrong preference target", change: func(reader *entryDetailReaderStub) { reader.preference.EntryID = 14 }, want: domain.ErrInvalidFileEntryData, displayCalls: 1, preferenceCalls: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, reader := entryDetailFixture()
			testCase.change(reader)
			details := NewEntryDetailService(func(ctx context.Context, action func(context.Context, EntryDetailReader) error) error {
				return action(ctx, reader)
			})
			result, err := details.Get(context.Background(), actor, 13)
			if result != nil || !errors.Is(err, testCase.want) || reader.displayCalls != testCase.displayCalls || reader.preferenceCalls != testCase.preferenceCalls {
				t.Fatal("详情故障、归属或读取短路错误", err, reader.displayCalls, reader.preferenceCalls)
			}
		})
	}
}

func TestEntryDetailServiceCopies(t *testing.T) {
	actor, reader := entryDetailFixture()
	details := NewEntryDetailService(func(ctx context.Context, action func(context.Context, EntryDetailReader) error) error {
		return action(ctx, reader)
	})
	first, err := details.Get(context.Background(), actor, 13)
	if err != nil {
		t.Fatal(err)
	}
	reader.afterPreference = func() {
		reader.entries[13].Name, *reader.entries[13].ParentID = "来源基础已变更", 99
		reader.display.ID, *reader.display.SourceDeviceID = 99, 99
		*reader.display.Category, *reader.display.Extension, *reader.display.MIMEType = "OTHER", "changed", "changed"
		*reader.display.SourceDeviceName, *reader.display.ColorKey, *reader.display.ContentModifiedAt = "changed", "changed", time.Time{}
	}
	second, err := details.Get(context.Background(), actor, 13)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("后续依赖修改来源影响了已复制的基础或展示字段", err)
	}
	*reader.preference.StarredAt, *reader.preference.LastOpenedAt = time.Time{}, time.Time{}
	if !reflect.DeepEqual(first, second) || first.Preference.StarredAt.IsZero() {
		t.Fatal("来源偏好与已返回结果共享时间指针")
	}
	second.Path.Entries[2].Name, *second.Path.Entries[2].ParentID = "本地基础修改", 88
	*second.Display.Category, *second.Display.Extension, *second.Display.MIMEType = "local", "local", "local"
	*second.Display.SourceDeviceID, *second.Display.SourceDeviceName, *second.Display.ColorKey = 88, "local", "local"
	*second.Display.ContentModifiedAt, *second.Preference.StarredAt, *second.Preference.LastOpenedAt = time.Time{}, time.Time{}, time.Time{}
	if first.Path.Entries[2].Name != "学习笔记.md" || *first.Path.Entries[2].ParentID != 12 || *first.Display.Category != "DOCUMENT" ||
		*first.Display.Extension != "md" || *first.Display.MIMEType != "text/markdown" || *first.Display.SourceDeviceID != 19 ||
		*first.Display.SourceDeviceName != "发布时设备 🎮" || *first.Display.ColorKey != "green" || first.Display.ContentModifiedAt.IsZero() ||
		first.Preference.StarredAt.IsZero() || first.Preference.LastOpenedAt.IsZero() {
		t.Fatal("修改某次详情影响了另一次结果")
	}
}

func TestEntryDetailServiceDepth(t *testing.T) {
	for _, depth := range []int{32, 33} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			actor, reader := entryDetailFixture()
			parentID := uint64(11)
			for level := 1; level <= depth; level++ {
				entry := *reader.entries[11]
				parent := parentID
				entry.ID, entry.ParentID, entry.Name = uint64(100+level), &parent, fmt.Sprint(level)
				reader.entries[entry.ID] = &entry
				parentID = entry.ID
			}
			reader.display = &domain.FileEntryDisplay{ID: parentID, UserID: actor.UserID}
			reader.preference = &domain.EntryPreference{UserID: actor.UserID, EntryID: parentID}
			details := NewEntryDetailService(func(ctx context.Context, action func(context.Context, EntryDetailReader) error) error {
				return action(ctx, reader)
			})
			result, err := details.Get(context.Background(), actor, parentID)
			if depth == 32 {
				if err != nil || result == nil || len(result.Path.Entries) != 33 || reader.displayCalls != 1 || reader.preferenceCalls != 1 {
					t.Fatal("合法最深层详情错误", err)
				}
			} else if result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) || reader.displayCalls != 0 || reader.preferenceCalls != 0 {
				t.Fatal("超深路径未在扩展读取前拒绝", err)
			}
		})
	}
}

func TestEntryDetailServiceInputAndSnapshot(t *testing.T) {
	for _, mode := range []string{
		"nil context", "zero user", "zero session", "zero device", "zero target", "cancelled",
		"after path", "after display", "after preference", "cancel with display failure", "cancel with preference failure",
		"display deadline", "preference deadline", "missing callback", "nil snapshot context", "nil reader", "end failure", "cancel after snapshot",
	} {
		t.Run(mode, func(t *testing.T) {
			actor, reader := entryDetailFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			targetID := uint64(13)
			wantSnapshots := 1
			var wantError error
			switch mode {
			case "nil context":
				ctx, wantSnapshots = nil, 0
			case "zero user":
				actor.UserID, wantSnapshots = 0, 0
			case "zero session":
				actor.SessionID, wantSnapshots = 0, 0
			case "zero device":
				actor.DeviceID, wantSnapshots = 0, 0
			case "zero target":
				targetID, wantSnapshots = 0, 0
			case "cancelled":
				cancel()
				wantSnapshots, wantError = 0, context.Canceled
			case "after path":
				reader.afterEntry = func(uint64) { cancel() }
				wantError = context.Canceled
			case "after display", "cancel with display failure":
				reader.afterDisplay = cancel
				wantError = context.Canceled
				if mode == "cancel with display failure" {
					reader.displayError = errors.New("fixture display cancellation")
				}
			case "after preference", "cancel with preference failure":
				reader.afterPreference = cancel
				wantError = context.Canceled
				if mode == "cancel with preference failure" {
					reader.preferenceError = errors.New("fixture preference cancellation")
				}
			case "display deadline", "preference deadline":
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 20*time.Millisecond)
				defer release()
				waitDeadline := func() { <-ctx.Done() }
				reader.afterDisplay = waitDeadline
				if mode == "preference deadline" {
					reader.afterDisplay, reader.afterPreference = nil, waitDeadline
				}
				wantError = context.DeadlineExceeded
			case "cancel after snapshot":
				wantError = context.Canceled
			}
			failure := errors.New("fixture snapshot end")
			snapshots := 0
			details := NewEntryDetailService(func(ctx context.Context, action func(context.Context, EntryDetailReader) error) error {
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
			if mode == "end failure" {
				wantError = failure
			}
			result, err := details.Get(ctx, actor, targetID)
			if result != nil || err == nil || snapshots != wantSnapshots || (wantError != nil && !errors.Is(err, wantError)) {
				t.Fatal("详情输入、取消或快照结束未拒绝结果", err, snapshots)
			}
			if (mode == "after path" && reader.displayCalls != 0) ||
				((mode == "after display" || mode == "cancel with display failure" || mode == "display deadline") && reader.preferenceCalls != 0) {
				t.Fatal("取消后仍执行了后续详情读取")
			}
		})
	}
}

func TestEntryDetailServiceDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少快照依赖仍构造详情服务")
		}
	}()
	NewEntryDetailService(nil)
}
