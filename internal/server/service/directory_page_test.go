package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type directoryPageStub struct {
	listed                                         *domain.DirectoryList
	position                                       *domain.FileEntryPosition
	verifyError, listError, signError              error
	nextCursor                                     string
	events                                         []string
	verifyUser, verifyParent, signUser, signParent uint64
	verifyText                                     string
	listContext                                    context.Context
	listActor                                      domain.Actor
	listParent                                     uint64
	listBefore                                     *domain.FileEntryPosition
	listSize                                       int
	signedPosition                                 domain.FileEntryPosition
	afterVerify, afterList, afterSign              func()
}

func (stub *directoryPageStub) verify(userID, parentID uint64, text string) (*domain.FileEntryPosition, error) {
	stub.events = append(stub.events, "verify")
	stub.verifyUser, stub.verifyParent, stub.verifyText = userID, parentID, text
	if stub.afterVerify != nil {
		stub.afterVerify()
	}
	return stub.position, stub.verifyError
}
func (stub *directoryPageStub) list(ctx context.Context, actor domain.Actor, parentID uint64, before *domain.FileEntryPosition, pageSize int) (*domain.DirectoryList, error) {
	stub.events = append(stub.events, "list")
	stub.listContext, stub.listActor, stub.listParent, stub.listBefore, stub.listSize = ctx, actor, parentID, before, pageSize
	if stub.afterList != nil {
		stub.afterList()
	}
	return stub.listed, stub.listError
}
func (stub *directoryPageStub) sign(userID, parentID uint64, position domain.FileEntryPosition) (string, error) {
	stub.events = append(stub.events, "sign")
	stub.signUser, stub.signParent, stub.signedPosition = userID, parentID, position
	if stub.afterSign != nil {
		stub.afterSign()
	}
	return stub.nextCursor, stub.signError
}

func directoryPageFixture() (domain.Actor, *directoryPageStub) {
	actor, reader := directoryListFixture()
	listed := &domain.DirectoryList{
		Path:    domain.FileEntryPath{Entries: []domain.FileEntry{*reader.entries[11], *reader.entries[12]}},
		Entries: reader.page.Entries, HasMore: true,
	}
	position := &domain.FileEntryPosition{ModifiedAt: listed.Entries[0].ModifiedAt.Add(time.Second).In(time.FixedZone("fixture", 8*3600)), ID: 20}
	return actor, &directoryPageStub{listed: listed, position: position, nextCursor: "fixture-next-cursor"}
}

func TestDirectoryPageServiceFlow(t *testing.T) {
	for _, mode := range []string{"first page", "next page", "last page", "empty page", "full last page", "root directory", "maximum page size"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备已有服务的结果，记录三个依赖的参数及调用顺序。
			actor, stub := directoryPageFixture()
			parentID, pageSize, cursor := uint64(12), 2, ""
			expectedEvents := "list,sign"
			switch mode {
			case "next page":
				cursor = "fixture-input-cursor"
				expectedEvents = "verify,list,sign"
			case "last page":
				stub.listed.Entries = stub.listed.Entries[:1]
				stub.listed.HasMore = false
				expectedEvents = "list"
			case "empty page":
				stub.listed.Entries = nil
				stub.listed.HasMore = false
				expectedEvents = "list"
			case "full last page":
				stub.listed.HasMore = false
				expectedEvents = "list"
			case "root directory":
				parentID = 11
				stub.listed.Path.Entries = stub.listed.Path.Entries[:1]
				stub.listed.Entries = nil
				stub.listed.HasMore = false
				expectedEvents = "list"
			case "maximum page size":
				pageSize = 100
				stub.listed.HasMore = false
				expectedEvents = "list"
			}
			type requestKey struct{}
			ctx := context.WithValue(context.Background(), requestKey{}, "request")
			pages := NewDirectoryPageService(stub.verify, stub.list, stub.sign)
			result, err := pages.List(ctx, actor, parentID, cursor, pageSize)
			if err != nil || result == nil || strings.Join(stub.events, ",") != expectedEvents || stub.listContext != ctx || stub.listActor != actor || stub.listParent != parentID || stub.listSize != pageSize {
				t.Fatal("分页服务流程或参数错误", err, stub.events)
			}
			if cursor == "" {
				if stub.listBefore != nil {
					t.Fatal("首页传入了分页位置")
				}
			} else if stub.verifyUser != actor.UserID || stub.verifyParent != parentID || stub.verifyText != cursor || stub.listBefore == nil || stub.listBefore == stub.position || !stub.listBefore.ModifiedAt.Equal(stub.position.ModifiedAt) || stub.listBefore.ModifiedAt.Location() != time.UTC || stub.listBefore.ID != stub.position.ID {
				t.Fatal("游标校验或位置传递错误")
			}

			// 2. 核对路径、空数组及可空游标，有下一页时签名位置为已交付末项。
			if !reflect.DeepEqual(result.Path, stub.listed.Path) || result.Entries == nil || len(result.Entries) != len(stub.listed.Entries) {
				t.Fatal("分页数据或空数组错误")
			}
			if stub.listed.HasMore {
				last := result.Entries[len(result.Entries)-1]
				if result.NextCursor == nil || *result.NextCursor != stub.nextCursor || stub.signUser != actor.UserID || stub.signParent != parentID || stub.signedPosition.ID != last.ID || !stub.signedPosition.ModifiedAt.Equal(last.ModifiedAt) {
					t.Fatal("下一页签名参数或结果错误")
				}
			} else if result.NextCursor != nil {
				t.Fatal("末页或空页仍生成下一页游标")
			}
		})
	}
}

func TestDirectoryPageServiceFailures(t *testing.T) {
	queryFailure, signFailure := errors.New("fixture list"), errors.New("fixture sign")
	for _, testCase := range []struct {
		name, cursor, events string
		change               func(*directoryPageStub)
		want                 error
	}{
		{name: "invalid cursor", cursor: "input", events: "verify", change: func(stub *directoryPageStub) { stub.verifyError = domain.ErrInvalidDirectoryCursor }, want: domain.ErrInvalidDirectoryCursor},
		{name: "nil position", cursor: "input", events: "verify", change: func(stub *directoryPageStub) { stub.position = nil }},
		{name: "bad position", cursor: "input", events: "verify", change: func(stub *directoryPageStub) { stub.position.ID = 0 }},
		{name: "list failure", events: "list", change: func(stub *directoryPageStub) { stub.listError = queryFailure }, want: queryFailure},
		{name: "hidden directory", events: "list", change: func(stub *directoryPageStub) { stub.listError = domain.ErrFileEntryNotFound }, want: domain.ErrFileEntryNotFound},
		{name: "file parent", events: "list", change: func(stub *directoryPageStub) { stub.listError = domain.ErrFileEntryNotDirectory }, want: domain.ErrFileEntryNotDirectory},
		{name: "disabled account", events: "list", change: func(stub *directoryPageStub) { stub.listError = domain.ErrAccountDisabled }, want: domain.ErrAccountDisabled},
		{name: "nil list", events: "list", change: func(stub *directoryPageStub) { stub.listed = nil }, want: domain.ErrInvalidFileEntryData},
		{name: "empty path", events: "list", change: func(stub *directoryPageStub) { stub.listed.Path.Entries = nil }, want: domain.ErrInvalidFileEntryData},
		{name: "wrong directory", events: "list", change: func(stub *directoryPageStub) { stub.listed.Path.Entries[1].ID = 99 }, want: domain.ErrInvalidFileEntryData},
		{name: "wrong owner", events: "list", change: func(stub *directoryPageStub) { stub.listed.Path.Entries[1].UserID = 8 }, want: domain.ErrInvalidFileEntryData},
		{name: "inactive parent", events: "list", change: func(stub *directoryPageStub) { stub.listed.Path.Entries[1].Status = "TRASHED" }, want: domain.ErrInvalidFileEntryData},
		{name: "file in path", events: "list", change: func(stub *directoryPageStub) { stub.listed.Path.Entries[1].EntryType = "FILE" }, want: domain.ErrInvalidFileEntryData},
		{name: "oversize list", events: "list", change: func(stub *directoryPageStub) {
			stub.listed.Entries = append(stub.listed.Entries, stub.listed.Entries[0])
		}, want: domain.ErrInvalidFileEntryData},
		{name: "short has more", events: "list", change: func(stub *directoryPageStub) { stub.listed.Entries = stub.listed.Entries[:1] }, want: domain.ErrInvalidFileEntryData},
		{name: "empty has more", events: "list", change: func(stub *directoryPageStub) { stub.listed.Entries = nil }, want: domain.ErrInvalidFileEntryData},
		{name: "bad last position", events: "list", change: func(stub *directoryPageStub) { stub.listed.Entries[1].ID = 0 }, want: domain.ErrInvalidFileEntryData},
		{name: "bad last owner", events: "list", change: func(stub *directoryPageStub) { stub.listed.Entries[1].UserID = 8 }, want: domain.ErrInvalidFileEntryData},
		{name: "bad last parent", events: "list", change: func(stub *directoryPageStub) { stub.listed.Entries[1].ParentID = nil }, want: domain.ErrInvalidFileEntryData},
		{name: "sign failure", events: "list,sign", change: func(stub *directoryPageStub) { stub.signError = signFailure }, want: signFailure},
		{name: "empty signature", events: "list,sign", change: func(stub *directoryPageStub) { stub.nextCursor = "" }},
		{name: "oversize signature", events: "list,sign", change: func(stub *directoryPageStub) {
			stub.nextCursor = strings.Repeat("a", domain.MaxDirectoryCursorLength+1)
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, stub := directoryPageFixture()
			testCase.change(stub)
			pages := NewDirectoryPageService(stub.verify, stub.list, stub.sign)
			result, err := pages.List(context.Background(), actor, 12, testCase.cursor, 2)
			if result != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || strings.Join(stub.events, ",") != testCase.events {
				t.Fatal("失败后返回了部分数据、丢失原因或未短路", err, stub.events)
			}
		})
	}
}

func TestDirectoryPageServiceCopies(t *testing.T) {
	actor, stub := directoryPageFixture()
	source := stub.listed
	stub.afterSign = func() {
		source.Path.Entries[1].Name = "签名期间修改路径"
		source.Entries[0].Name = "签名期间修改子项"
		*source.Entries[0].ParentID = 99
	}
	pages := NewDirectoryPageService(stub.verify, stub.list, stub.sign)
	first, err := pages.List(context.Background(), actor, 12, "", 2)
	if err != nil || first == nil {
		t.Fatal(err)
	}
	if first.Path.Entries[1].Name != "课程资料 🌱" || first.Entries[0].Name != "学习笔记.md" || *first.Entries[0].ParentID != 12 {
		t.Fatal("签名依赖修改来源影响了结果")
	}
	stub.afterSign = nil
	_, fresh := directoryPageFixture()
	stub.listed = fresh.listed
	second, err := pages.List(context.Background(), actor, 12, "", 2)
	if err != nil || second == nil {
		t.Fatal(err)
	}
	first.Path.Entries[1].Name, first.Entries[0].Name, *first.Entries[0].ParentID, *first.NextCursor = "本地路径", "本地子项", 88, "本地游标"
	if second.Path.Entries[1].Name != "课程资料 🌱" || second.Entries[0].Name != "学习笔记.md" || *second.Entries[0].ParentID != 12 || *second.NextCursor != "fixture-next-cursor" {
		t.Fatal("返回结果共享了路径、子项或游标指针")
	}
}

func TestDirectoryPageServiceInputsAndCancellation(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero session", "zero device", "zero parent", "zero size", "negative size", "oversize", "long cursor", "cancelled", "after verify", "after list", "after sign", "verify failure and cancel", "list failure and cancel", "sign failure and cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			actor, stub := directoryPageFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			parentID, pageSize, cursor := uint64(12), 2, ""
			wantEvents := ""
			var want error
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
			case "negative size":
				pageSize = -1
			case "oversize":
				pageSize = 101
			case "long cursor":
				cursor = strings.Repeat("x", domain.MaxDirectoryCursorLength+1)
				want = domain.ErrInvalidDirectoryCursor
			case "cancelled":
				cancel()
				want = context.Canceled
			case "after verify", "verify failure and cancel":
				cursor = "input"
				stub.afterVerify = cancel
				want = context.Canceled
				wantEvents = "verify"
			case "after list", "list failure and cancel":
				stub.afterList = cancel
				want = context.Canceled
				wantEvents = "list"
			case "after sign", "sign failure and cancel":
				stub.afterSign = cancel
				want = context.Canceled
				wantEvents = "list,sign"
			case "deadline":
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 20*time.Millisecond)
				defer release()
				stub.afterList = func() { <-ctx.Done() }
				want = context.DeadlineExceeded
				wantEvents = "list"
			}
			if strings.Contains(mode, "failure") {
				stub.verifyError, stub.listError, stub.signError = errors.New("verify"), errors.New("list"), errors.New("sign")
				if strings.HasPrefix(mode, "list") {
					stub.verifyError = nil
				}
				if strings.HasPrefix(mode, "sign") {
					stub.verifyError, stub.listError = nil, nil
				}
			}
			pages := NewDirectoryPageService(stub.verify, stub.list, stub.sign)
			result, err := pages.List(ctx, actor, parentID, cursor, pageSize)
			if result != nil || err == nil || (want != nil && !errors.Is(err, want)) || strings.Join(stub.events, ",") != wantEvents {
				t.Fatal("输入或取消处理错误", err, stub.events)
			}
		})
	}
}

func TestDirectoryPageServiceDependencies(t *testing.T) {
	for _, missing := range []string{"verify", "list", "sign"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖仍构造成功")
				}
			}()
			_, stub := directoryPageFixture()
			verify, list, sign := stub.verify, stub.list, stub.sign
			switch missing {
			case "verify":
				verify = nil
			case "list":
				list = nil
			case "sign":
				sign = nil
			}
			NewDirectoryPageService(verify, list, sign)
		})
	}
}
