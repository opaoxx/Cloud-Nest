package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type createDirectoryTransactionStub struct {
	*fileEntryPathReaderStub
	entry         *domain.FileEntry
	createError   error
	createCalls   int
	createContext context.Context
	parentID      uint64
	name          string
	nameKey       []byte
	colorKey      *string
	afterCreate   func()
}

func (transaction *createDirectoryTransactionStub) CreateUserDirectory(ctx context.Context, parentID uint64, name string, nameKey []byte, colorKey *string) (*domain.FileEntry, error) {
	transaction.createCalls++
	transaction.createContext, transaction.parentID, transaction.name = ctx, parentID, name
	transaction.nameKey = bytes.Clone(nameKey)
	transaction.colorKey = colorKey
	if transaction.afterCreate != nil {
		transaction.afterCreate()
	}
	return transaction.entry, transaction.createError
}

func createDirectoryFixture() (domain.Actor, *createDirectoryTransactionStub) {
	actor, reader := fileEntryPathFixture()
	parentID := uint64(12)
	stamp := reader.entries[11].CreatedAt
	return actor, &createDirectoryTransactionStub{
		fileEntryPathReaderStub: reader,
		entry: &domain.FileEntry{
			ID: 23, UserID: actor.UserID, ParentID: &parentID, Name: "Cafe\u0301", EntryType: "DIR",
			Revision: 1, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: stamp, UpdatedAt: stamp,
		},
	}
}

func TestCreateDirectoryServiceResults(t *testing.T) {
	for _, mode := range []string{"normal", "root parent", "null color", "empty color", "full unsigned generated id", "full unsigned actor and parent", "unicode expansion"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction := createDirectoryFixture()
			parentID, rawName, key := uint64(12), "  Cafe\u0301  ", "café"
			color := "green"
			colorKey := &color
			switch mode {
			case "root parent":
				parentID = 11
				transaction.entry.ParentID = &parentID
			case "null color":
				colorKey = nil
			case "empty color":
				color = ""
			case "full unsigned generated id":
				transaction.entry.ID = ^uint64(0)
			case "full unsigned actor and parent":
				maximum := ^uint64(0)
				actor = domain.Actor{UserID: maximum, SessionID: maximum, DeviceID: maximum}
				transaction.profile.ID = maximum
				for _, entry := range transaction.entries {
					entry.UserID = maximum
				}
				parentID = maximum
				directory := transaction.entries[12]
				delete(transaction.entries, 12)
				directory.ID = parentID
				transaction.entries[parentID] = directory
				transaction.entry.ID, transaction.entry.UserID, transaction.entry.ParentID = maximum-1, maximum, &parentID
			case "unicode expansion":
				rawName = strings.Repeat("\u0f73", 120)
				transaction.entry.Name = rawName
				key = strings.Repeat("\u0f71", 120) + strings.Repeat("\u0f72", 120)
			}
			requestContext := context.Background()
			type transactionKey struct{}
			transactionContext := context.WithValue(requestContext, transactionKey{}, "write transaction")
			calls := 0
			creator := NewCreateDirectoryService(func(ctx context.Context, current domain.Actor, action func(context.Context, CreateDirectoryTransaction) error) error {
				calls++
				if ctx != requestContext || current != actor {
					t.Error("事务入口未收到准确上下文与 Actor")
				}
				return action(transactionContext, transaction)
			})
			entry, err := creator.Create(requestContext, actor, parentID, rawName, colorKey)
			if err != nil || !reflect.DeepEqual(entry, transaction.entry) || calls != 1 || transaction.createCalls != 1 || transaction.createContext != transactionContext || transaction.parentID != parentID || transaction.name != transaction.entry.Name || string(transaction.nameKey) != key {
				t.Fatal("创建目录结果、参数或单次事务错误", err)
			}
			if (transaction.colorKey == nil) != (colorKey == nil) || (colorKey != nil && (*transaction.colorKey != *colorKey || transaction.colorKey == colorKey)) {
				t.Fatal("颜色空值、原文或独立副本错误")
			}
			for index, ctx := range transaction.contexts {
				if ctx != transactionContext || transaction.userIDs[index] != actor.UserID {
					t.Fatal("路径读取未复用写事务上下文和用户")
				}
			}
		})
	}
}

func TestCreateDirectoryServiceFailures(t *testing.T) {
	queryFailure := errors.New("fixture directory query")
	for _, testCase := range []struct {
		name   string
		change func(*createDirectoryTransactionStub)
		want   error
	}{
		{name: "missing parent", want: domain.ErrFileEntryNotFound, change: func(transaction *createDirectoryTransactionStub) { delete(transaction.entries, 12) }},
		{name: "hidden ancestor", want: domain.ErrFileEntryNotFound, change: func(transaction *createDirectoryTransactionStub) { transaction.entries[11].Status = "TRASHED" }},
		{name: "disabled profile", want: domain.ErrAccountDisabled, change: func(transaction *createDirectoryTransactionStub) { transaction.profile.Status = "DISABLED" }},
		{name: "broken parent chain", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entries[12].ParentID = nil }},
		{name: "path query failure", want: queryFailure, change: func(transaction *createDirectoryTransactionStub) { transaction.entryErrors[12] = queryFailure }},
		{name: "name conflict", want: domain.ErrEntryNameConflict, change: func(transaction *createDirectoryTransactionStub) {
			transaction.createError = domain.ErrEntryNameConflict
		}},
		{name: "write failure", want: queryFailure, change: func(transaction *createDirectoryTransactionStub) { transaction.createError = queryFailure }},
		{name: "nil record", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entry = nil }},
		{name: "zero id", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entry.ID = 0 }},
		{name: "wrong owner", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entry.UserID++ }},
		{name: "nil parent", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entry.ParentID = nil }},
		{name: "wrong parent", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { *transaction.entry.ParentID = 99 }},
		{name: "wrong name", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entry.Name = "other" }},
		{name: "wrong type", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entry.EntryType = "FILE" }},
		{name: "wrong status", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entry.Status = "TRASHED" }},
		{name: "nonzero size", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entry.FileSize = 1 }},
		{name: "wrong revision", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entry.Revision = 2 }},
		{name: "zero created time", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entry.CreatedAt = time.Time{} }},
		{name: "zero modified time", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entry.ModifiedAt = time.Time{} }},
		{name: "zero updated time", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entry.UpdatedAt = time.Time{} }},
		{name: "id equals parent", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entry.ID = 12 }},
		{name: "id equals ancestor", want: domain.ErrInvalidFileEntryData, change: func(transaction *createDirectoryTransactionStub) { transaction.entry.ID = 11 }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, transaction := createDirectoryFixture()
			testCase.change(transaction)
			creator := NewCreateDirectoryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, CreateDirectoryTransaction) error) error {
				return action(ctx, transaction)
			})
			entry, err := creator.Create(context.Background(), actor, 12, "Cafe\u0301", nil)
			if entry != nil || !errors.Is(err, testCase.want) {
				t.Fatal("失败未返回空结果与正确错误", err)
			}
			if strings.HasPrefix(testCase.name, "missing") || testCase.name == "hidden ancestor" || testCase.name == "disabled profile" || testCase.name == "broken parent chain" || testCase.name == "path query failure" {
				if transaction.createCalls != 0 {
					t.Fatal("路径检查失败后仍执行目录写入")
				}
			}
		})
	}
}

func TestCreateDirectoryServiceParentAndDepth(t *testing.T) {
	actor, transaction := createDirectoryFixture()
	creator := NewCreateDirectoryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, CreateDirectoryTransaction) error) error {
		return action(ctx, transaction)
	})
	if entry, err := creator.Create(context.Background(), actor, 13, "new", nil); entry != nil || !errors.Is(err, domain.ErrFileEntryNotDirectory) || transaction.createCalls != 0 {
		t.Fatal("文件父项未拒绝写入", err)
	}
	for _, depth := range []int{31, 32, 33} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			actor, transaction := createDirectoryFixture()
			parentID := uint64(11)
			for level := 1; level <= depth; level++ {
				parent := parentID
				entry := *transaction.entries[11]
				entry.ID, entry.ParentID, entry.Name = uint64(100+level), &parent, fmt.Sprint(level)
				transaction.entries[entry.ID] = &entry
				parentID = entry.ID
			}
			transaction.entry.ParentID = &parentID
			creator := NewCreateDirectoryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, CreateDirectoryTransaction) error) error {
				return action(ctx, transaction)
			})
			entry, err := creator.Create(context.Background(), actor, parentID, "Cafe\u0301", nil)
			if depth == 31 {
				if err != nil || entry == nil || transaction.createCalls != 1 {
					t.Fatal("合法第 32 层创建失败", err)
				}
				return
			}
			want := domain.ErrFileEntryDepthExceeded
			if depth == 33 {
				want = domain.ErrInvalidFileEntryData
			}
			if entry != nil || !errors.Is(err, want) || transaction.createCalls != 0 {
				t.Fatal("深度越界或异常父链仍允许写入", err)
			}
		})
	}
}

func TestCreateDirectoryServiceInput(t *testing.T) {
	actor, _ := createDirectoryFixture()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	invalidColor := "green\n"
	for _, testCase := range []struct {
		name     string
		ctx      context.Context
		actor    domain.Actor
		parentID uint64
		rawName  string
		color    *string
		want     error
	}{
		{name: "nil context", actor: actor, parentID: 12, rawName: "new"},
		{name: "missing actor", ctx: context.Background(), parentID: 12, rawName: "new"},
		{name: "missing device", ctx: context.Background(), actor: domain.Actor{UserID: 7, SessionID: 17}, parentID: 12, rawName: "new"},
		{name: "missing session", ctx: context.Background(), actor: domain.Actor{UserID: 7, DeviceID: 19}, parentID: 12, rawName: "new"},
		{name: "zero parent", ctx: context.Background(), actor: actor, rawName: "new"},
		{name: "cancelled", ctx: cancelled, actor: actor, parentID: 12, rawName: "new", want: context.Canceled},
		{name: "empty name", ctx: context.Background(), actor: actor, parentID: 12, want: domain.ErrInvalidEntryName},
		{name: "path name", ctx: context.Background(), actor: actor, parentID: 12, rawName: "a/b", want: domain.ErrInvalidEntryName},
		{name: "reserved name", ctx: context.Background(), actor: actor, parentID: 12, rawName: "CON.txt", want: domain.ErrInvalidEntryName},
		{name: "long emoji", ctx: context.Background(), actor: actor, parentID: 12, rawName: strings.Repeat("🌱", 61), want: domain.ErrInvalidEntryName},
		{name: "invalid color", ctx: context.Background(), actor: actor, parentID: 12, rawName: "new", color: &invalidColor, want: domain.ErrInvalidDirectoryColorKey},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			calls := 0
			creator := NewCreateDirectoryService(func(context.Context, domain.Actor, func(context.Context, CreateDirectoryTransaction) error) error {
				calls++
				return nil
			})
			entry, err := creator.Create(testCase.ctx, testCase.actor, testCase.parentID, testCase.rawName, testCase.color)
			if entry != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || calls != 0 {
				t.Fatal("非法输入仍进入事务或错误类别不符", err)
			}
		})
	}
}

func TestCreateDirectoryServiceTransactionAndCopies(t *testing.T) {
	for _, mode := range []string{"entry failure", "commit failure", "missing callback", "nil context", "nil transaction", "cancel before path", "cancel after create", "cancel after commit", "copies"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction := createDirectoryFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			color := "green"
			failure := errors.New("fixture transaction failure")
			creator := NewCreateDirectoryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, CreateDirectoryTransaction) error) error {
				switch mode {
				case "entry failure":
					return failure
				case "missing callback":
					return nil
				case "nil context":
					return action(nil, transaction)
				case "nil transaction":
					return action(ctx, nil)
				case "cancel before path":
					cancel()
				case "cancel after create":
					transaction.afterCreate = cancel
				case "copies":
					color = "blue"
				}
				if err := action(ctx, transaction); err != nil {
					return err
				}
				if mode == "commit failure" {
					return failure
				}
				if mode == "cancel after commit" {
					cancel()
				}
				if mode == "copies" {
					transaction.entry.Name = "mutated after callback"
					*transaction.entry.ParentID = 999
				}
				return nil
			})
			entry, err := creator.Create(ctx, actor, 12, "Cafe\u0301", &color)
			if mode == "cancel after commit" || mode == "copies" {
				if err != nil || entry == nil || entry.Name != "Cafe\u0301" || *entry.ParentID != 12 || *transaction.colorKey != "green" {
					t.Fatal("已提交结果或独立副本错误", err)
				}
				return
			}
			if entry != nil || err == nil {
				t.Fatal("事务异常仍返回准备结果")
			}
			if (mode == "entry failure" || mode == "commit failure") && !errors.Is(err, failure) {
				t.Fatal("事务错误原因丢失", err)
			}
			if strings.HasPrefix(mode, "cancel") && !errors.Is(err, context.Canceled) {
				t.Fatal("事务取消原因丢失", err)
			}
		})
	}
}

func TestCreateDirectoryServiceDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("缺少事务方法未拒绝装配")
		}
	}()
	NewCreateDirectoryService(nil)
}
