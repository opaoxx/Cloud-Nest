package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func entryMoveTargetFixture() (domain.Actor, *fileEntryPathReaderStub) {
	actor, reader := fileEntryPathFixture()
	rootID, directoryID, childID := uint64(11), uint64(12), uint64(15)
	sibling, child, grandchild := *reader.entries[12], *reader.entries[12], *reader.entries[12]
	sibling.ID, sibling.ParentID, sibling.Name = 14, &rootID, "课程资料的副本"
	child.ID, child.ParentID, child.Name = childID, &directoryID, "子目录"
	grandchild.ID, grandchild.ParentID, grandchild.Name = 16, &childID, "孙目录"
	reader.entries[14], reader.entries[15], reader.entries[16] = &sibling, &child, &grandchild
	return actor, reader
}

func TestEntryMoveTargetServiceResults(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		source, target uint64
	}{
		{"file to sibling", 13, 14}, {"file to root", 13, 11}, {"file current parent", 13, 12},
		{"directory current parent", 12, 11}, {"directory to sibling", 12, 14}, {"file to nested sibling", 13, 15},
		{"directory to ancestor", 15, 11}, {"nested current parent", 15, 12}, {"maximum identifiers", 13, 14},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, reader := entryMoveTargetFixture()
			source, target := testCase.source, testCase.target
			if testCase.name == "maximum identifiers" {
				maximum := ^uint64(0)
				actor = domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}
				reader.profile.ID = maximum
				for _, entry := range reader.entries {
					entry.UserID = maximum
				}
				file, sibling := reader.entries[13], reader.entries[14]
				delete(reader.entries, 13)
				delete(reader.entries, 14)
				source, target = maximum, maximum-1
				file.ID, sibling.ID = source, target
				reader.entries[source], reader.entries[target] = file, sibling
			}
			requestContext := context.Background()
			type snapshotKey struct{}
			snapshotContext := context.WithValue(requestContext, snapshotKey{}, "move paths")
			calls := 0
			checker := NewEntryMoveTargetService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
				calls++
				if ctx != requestContext {
					t.Error("读取入口上下文错误")
				}
				return action(snapshotContext, reader)
			})
			result, err := checker.Check(requestContext, actor, source, target)
			if err != nil || result == nil || calls != 1 || result.Source.Entries[len(result.Source.Entries)-1].ID != source || result.TargetParent.Entries[len(result.TargetParent.Entries)-1].ID != target {
				t.Fatal("合法移动关系或单次读取范围错误", err)
			}
			for index, ctx := range reader.contexts {
				if ctx != snapshotContext || reader.userIDs[index] != actor.UserID {
					t.Fatal("路径读取未复用上下文与用户")
				}
			}
		})
	}
}

func TestEntryMoveTargetServiceFailures(t *testing.T) {
	failure := errors.New("fixture move path failure")
	for _, testCase := range []struct {
		name           string
		source, target uint64
		change         func(*fileEntryPathReaderStub)
		want           error
	}{
		{"missing source", 999, 14, nil, domain.ErrFileEntryNotFound},
		{"missing target", 13, 999, nil, domain.ErrFileEntryNotFound},
		{"root source", 11, 14, nil, domain.ErrRootEntryMove},
		{"target file", 14, 13, nil, domain.ErrFileEntryNotDirectory},
		{"file itself", 13, 13, nil, domain.ErrFileEntryNotDirectory},
		{"directory itself", 12, 12, nil, domain.ErrEntryMoveIntoSubtree},
		{"direct descendant", 12, 15, nil, domain.ErrEntryMoveIntoSubtree},
		{"deep descendant", 12, 16, nil, domain.ErrEntryMoveIntoSubtree},
		{"hidden source", 13, 14, func(reader *fileEntryPathReaderStub) { reader.entries[13].Status = "TRASHED" }, domain.ErrFileEntryNotFound},
		{"hidden source ancestor", 13, 14, func(reader *fileEntryPathReaderStub) { reader.entries[12].Status = "PURGING" }, domain.ErrFileEntryNotFound},
		{"hidden target", 13, 14, func(reader *fileEntryPathReaderStub) { reader.entries[14].Status = "PURGED" }, domain.ErrFileEntryNotFound},
		{"hidden target ancestor", 14, 15, func(reader *fileEntryPathReaderStub) { reader.entries[12].Status = "TRASHED" }, domain.ErrFileEntryNotFound},
		{"foreign source data", 13, 14, func(reader *fileEntryPathReaderStub) { reader.entries[13].UserID++ }, domain.ErrInvalidFileEntryData},
		{"foreign target data", 13, 14, func(reader *fileEntryPathReaderStub) { reader.entries[14].UserID++ }, domain.ErrInvalidFileEntryData},
		{"disabled profile", 13, 14, func(reader *fileEntryPathReaderStub) { reader.profile.Status = "DISABLED" }, domain.ErrAccountDisabled},
		{"broken source chain", 13, 14, func(reader *fileEntryPathReaderStub) { reader.entries[13].ParentID = nil }, domain.ErrInvalidFileEntryData},
		{"broken target chain", 13, 14, func(reader *fileEntryPathReaderStub) { parent := uint64(0); reader.entries[14].ParentID = &parent }, domain.ErrInvalidFileEntryData},
		{"source query failure", 13, 14, func(reader *fileEntryPathReaderStub) { reader.entryErrors[13] = failure }, failure},
		{"target query failure", 13, 14, func(reader *fileEntryPathReaderStub) { reader.entryErrors[14] = failure }, failure},
		{"different observed roots", 13, 14, func(reader *fileEntryPathReaderStub) {
			otherRoot := *reader.entries[11]
			otherRoot.ID = 91
			reader.entries[91] = &otherRoot
			reader.afterEntry = func(id uint64) {
				if id == 11 {
					reader.profile.RootEntryID = 91
					parent := uint64(91)
					reader.entries[14].ParentID = &parent
				}
			}
		}, domain.ErrInvalidFileEntryData},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, reader := entryMoveTargetFixture()
			if testCase.change != nil {
				testCase.change(reader)
			}
			checker := NewEntryMoveTargetService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
				return action(ctx, reader)
			})
			result, err := checker.Check(context.Background(), actor, testCase.source, testCase.target)
			if result != nil || !errors.Is(err, testCase.want) {
				t.Fatal("异常移动关系或错误链错误", err)
			}
			if testCase.name == "root source" && !reflect.DeepEqual(reader.entryIDs, []uint64{11}) {
				t.Fatal("根目录拒绝后继续读取目的地")
			}
		})
	}
}

func TestEntryMoveTargetServiceBoundaries(t *testing.T) {
	failure := errors.New("fixture snapshot failure")
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "zero source", "zero target", "cancelled input", "nil read context", "nil reader", "entry failure", "completion failure", "no callback", "cancel source", "cancel target", "cancel after completion"} {
		t.Run(mode, func(t *testing.T) {
			actor, reader := entryMoveTargetFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			source, target := uint64(13), uint64(14)
			var want error
			switch mode {
			case "nil context":
				callContext = nil
			case "zero user":
				actor.UserID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero session":
				actor.SessionID = 0
			case "zero source":
				source = 0
			case "zero target":
				target = 0
			case "cancelled input":
				cancel()
				want = context.Canceled
			case "entry failure", "completion failure":
				want = failure
			case "no callback":
				want = domain.ErrInvalidFileEntryData
			case "cancel source":
				reader.afterEntry = func(id uint64) {
					if id == 13 {
						cancel()
					}
				}
				want = context.Canceled
			case "cancel target":
				reader.afterEntry = func(id uint64) {
					if id == 14 {
						cancel()
					}
				}
				want = context.Canceled
			case "cancel after completion":
				want = context.Canceled
			}
			calls := 0
			checker := NewEntryMoveTargetService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
				calls++
				switch mode {
				case "nil read context":
					return action(nil, reader)
				case "nil reader":
					return action(ctx, nil)
				case "entry failure":
					return failure
				case "no callback":
					return nil
				}
				if err := action(ctx, reader); err != nil {
					return err
				}
				if mode == "completion failure" {
					return failure
				}
				if mode == "cancel after completion" {
					cancel()
				}
				return nil
			})
			result, err := checker.Check(callContext, actor, source, target)
			if result != nil || err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatal("输入或读取边界未处理", err)
			}
			if mode == "nil context" || strings.HasPrefix(mode, "zero ") || mode == "cancelled input" {
				if calls != 0 {
					t.Fatal("非法输入进入读取范围")
				}
			}
		})
	}
	t.Run("missing dependency", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("缺少路径入口未拒绝")
			}
		}()
		NewEntryMoveTargetService(nil)
	})
}

func TestEntryMoveTargetServiceCopiesAndConcurrent(t *testing.T) {
	actor, reader := entryMoveTargetFixture()
	checker := NewEntryMoveTargetService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
		return action(ctx, reader)
	})
	before, err := json.Marshal(reader.entries)
	if err != nil {
		t.Fatal(err)
	}
	first, err := checker.Check(context.Background(), actor, 13, 12)
	if err != nil {
		t.Fatal(err)
	}
	second, err := checker.Check(context.Background(), actor, 13, 12)
	if err != nil || first == second || !reflect.DeepEqual(first, second) {
		t.Fatal("关系结果未独立返回", err)
	}
	after, err := json.Marshal(reader.entries)
	if err != nil || string(before) != string(after) {
		t.Fatal("关系检查修改了原记录", err)
	}
	*first.Source.Entries[1].ParentID = 999
	first.Source.Entries[2].Name = "local name"
	if *first.TargetParent.Entries[1].ParentID != 11 || *second.Source.Entries[1].ParentID != 11 || reader.entries[13].Name != second.Source.Entries[2].Name {
		t.Fatal("路径或父目录指针共享可变内存")
	}
	var workers sync.WaitGroup
	sharedChecker := NewEntryMoveTargetService(func(ctx context.Context, action func(context.Context, FileEntryPathReader) error) error {
		// 每次调用取得独立读取范围，共享服务实例保留不可变依赖。
		_, currentReader := entryMoveTargetFixture()
		return action(ctx, currentReader)
	})
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				result, err := sharedChecker.Check(context.Background(), actor, 13, 14)
				if err != nil || result == nil {
					t.Error("并发关系检查失败", err)
					return
				}
			}
		}()
	}
	workers.Wait()
}
