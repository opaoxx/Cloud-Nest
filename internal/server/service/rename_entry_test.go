package service

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type renameEntryTransactionStub struct {
	*fileEntryPathReaderStub
	result       *domain.FileEntry
	writeError   error
	writeCalls   int
	writeContext context.Context
	entryID      uint64
	revision     uint64
	metadata     domain.EntryNameMetadata
	nameKey      []byte
	afterWrite   func()
}

func (transaction *renameEntryTransactionStub) RenameUserFileEntry(ctx context.Context, entryID, expectedRevision uint64, metadata domain.EntryNameMetadata, nameKey []byte) (*domain.FileEntry, error) {
	transaction.writeCalls++
	transaction.writeContext, transaction.entryID, transaction.revision = ctx, entryID, expectedRevision
	transaction.metadata, transaction.nameKey = metadata, bytes.Clone(nameKey)
	if transaction.afterWrite != nil {
		transaction.afterWrite()
	}
	return transaction.result, transaction.writeError
}

func renameEntryServiceFixture() (domain.Actor, *renameEntryTransactionStub) {
	actor, reader := fileEntryPathFixture()
	result := *reader.entries[13]
	result.ParentID = copyOptionalValue(result.ParentID)
	result.Name, result.Revision = "Cafe\u0301.PDF", result.Revision+1
	result.ModifiedAt, result.UpdatedAt = result.ModifiedAt.Add(time.Minute), result.UpdatedAt.Add(time.Minute)
	return actor, &renameEntryTransactionStub{fileEntryPathReaderStub: reader, result: &result}
}

func TestRenameEntryServiceResults(t *testing.T) {
	for _, mode := range []string{"file", "directory", "same name", "case change", "canonical spelling", "maximum same name", "maximum increment", "high unsigned identifiers", "long suffix"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction := renameEntryServiceFixture()
			target := transaction.entries[13]
			name, key, extension, category := "  Cafe\u0301.PDF  ", "café.pdf", "pdf", "DOCUMENT"
			switch mode {
			case "directory":
				target.EntryType, target.FileSize = "DIR", 0
				transaction.result.EntryType, transaction.result.FileSize = "DIR", 0
			case "same name", "maximum same name":
				target.Name = "Cafe\u0301.PDF"
				if mode == "maximum same name" {
					target.Revision = ^uint64(0)
				}
				result := *target
				transaction.result = &result
			case "case change":
				target.Name = "cafe\u0301.pdf"
			case "canonical spelling":
				target.Name = "Café.PDF"
			case "maximum increment":
				target.Revision, transaction.result.Revision = ^uint64(0)-1, ^uint64(0)
			case "high unsigned identifiers":
				maximum := ^uint64(0)
				actor = domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}
				transaction.profile.ID = maximum
				for _, entry := range transaction.entries {
					entry.UserID = maximum
				}
				delete(transaction.entries, 13)
				target.ID = maximum
				transaction.entries[maximum] = target
				transaction.result.ID, transaction.result.UserID = maximum, maximum
			case "long suffix":
				name = "资料." + strings.Repeat("🌱", 33)
				key, category = name, "OTHER"
				transaction.result.Name = name
			}
			requestContext := context.Background()
			type transactionKey struct{}
			txContext := context.WithValue(requestContext, transactionKey{}, "rename")
			calls := 0
			renamer := NewRenameEntryService(func(ctx context.Context, current domain.Actor, action func(context.Context, RenameEntryTransaction) error) error {
				calls++
				if ctx != requestContext || current != actor {
					t.Error("事务入口的上下文或身份错误")
				}
				return action(txContext, transaction)
			})
			entry, err := renamer.Rename(requestContext, actor, target.ID, target.Revision, name)
			if err != nil || !reflect.DeepEqual(entry, transaction.result) || calls != 1 || transaction.writeCalls != 1 || transaction.entryID != target.ID || transaction.revision != target.Revision || transaction.writeContext != txContext || string(transaction.nameKey) != key {
				t.Fatal("改名结果、参数或单次事务错误", err)
			}
			metadata := transaction.metadata
			if metadata.Name != entry.Name {
				t.Fatal("展示名称错误")
			}
			if mode == "directory" {
				if metadata.Category != nil || metadata.Extension != nil {
					t.Fatal("文件夹派生字段错误")
				}
			} else if metadata.Category == nil || *metadata.Category != category || (mode == "long suffix" && metadata.Extension != nil) || (mode != "long suffix" && (metadata.Extension == nil || *metadata.Extension != extension)) {
				t.Fatal("文件分类或后缀错误")
			}
			for index, ctx := range transaction.contexts {
				if ctx != txContext || transaction.userIDs[index] != actor.UserID {
					t.Fatal("路径读取未复用事务上下文和用户")
				}
			}
			if entry == transaction.result || entry.ParentID == transaction.result.ParentID {
				t.Fatal("输出未复制")
			}
			*transaction.result.ParentID = 999
			transaction.result.Name = "later mutation"
			if *entry.ParentID != 12 || entry.Name == "later mutation" {
				t.Fatal("结果受到事务对象后续修改影响")
			}
		})
	}
}

func TestRenameEntryServiceFailures(t *testing.T) {
	failure := errors.New("fixture rename failure")
	for _, testCase := range []struct {
		name   string
		change func(*renameEntryTransactionStub)
		want   error
		writes int
	}{
		{"missing target", func(tx *renameEntryTransactionStub) { delete(tx.entries, 13) }, domain.ErrFileEntryNotFound, 0},
		{"hidden target", func(tx *renameEntryTransactionStub) { tx.entries[13].Status = "TRASHED" }, domain.ErrFileEntryNotFound, 0},
		{"hidden ancestor", func(tx *renameEntryTransactionStub) { tx.entries[12].Status = "PURGED" }, domain.ErrFileEntryNotFound, 0},
		{"disabled", func(tx *renameEntryTransactionStub) { tx.profile.Status = "DISABLED" }, domain.ErrAccountDisabled, 0},
		{"foreign target", func(tx *renameEntryTransactionStub) { tx.entries[13].UserID++ }, domain.ErrInvalidFileEntryData, 0},
		{"broken chain", func(tx *renameEntryTransactionStub) { tx.entries[12].ParentID = nil }, domain.ErrInvalidFileEntryData, 0},
		{"query failure", func(tx *renameEntryTransactionStub) { tx.entryErrors[12] = failure }, failure, 0},
		{"revision conflict", func(tx *renameEntryTransactionStub) { tx.entries[13].Revision++ }, domain.ErrEntryRevisionConflict, 0},
		{"same name stale version", func(tx *renameEntryTransactionStub) {
			tx.entries[13].Name, tx.entries[13].Revision = "Cafe\u0301.PDF", 4
		}, domain.ErrEntryRevisionConflict, 0},
		{"name conflict", func(tx *renameEntryTransactionStub) { tx.writeError = domain.ErrEntryNameConflict }, domain.ErrEntryNameConflict, 1},
		{"locked revision conflict", func(tx *renameEntryTransactionStub) {
			tx.writeError = &domain.EntryRevisionConflictError{CurrentRevision: 4}
		}, domain.ErrEntryRevisionConflict, 1},
		{"write failure", func(tx *renameEntryTransactionStub) { tx.writeError = failure }, failure, 1},
		{"nil result", func(tx *renameEntryTransactionStub) { tx.result = nil }, domain.ErrInvalidFileEntryData, 1},
		{"wrong id", func(tx *renameEntryTransactionStub) { tx.result.ID++ }, domain.ErrInvalidFileEntryData, 1},
		{"wrong owner", func(tx *renameEntryTransactionStub) { tx.result.UserID++ }, domain.ErrInvalidFileEntryData, 1},
		{"nil parent", func(tx *renameEntryTransactionStub) { tx.result.ParentID = nil }, domain.ErrInvalidFileEntryData, 1},
		{"wrong parent", func(tx *renameEntryTransactionStub) { *tx.result.ParentID = 99 }, domain.ErrInvalidFileEntryData, 1},
		{"wrong name", func(tx *renameEntryTransactionStub) { tx.result.Name = "other" }, domain.ErrInvalidFileEntryData, 1},
		{"wrong type", func(tx *renameEntryTransactionStub) { tx.result.EntryType = "DIR" }, domain.ErrInvalidFileEntryData, 1},
		{"wrong size", func(tx *renameEntryTransactionStub) { tx.result.FileSize++ }, domain.ErrInvalidFileEntryData, 1},
		{"wrong status", func(tx *renameEntryTransactionStub) { tx.result.Status = "TRASHED" }, domain.ErrInvalidFileEntryData, 1},
		{"wrong revision", func(tx *renameEntryTransactionStub) { tx.result.Revision++ }, domain.ErrInvalidFileEntryData, 1},
		{"changed creation", func(tx *renameEntryTransactionStub) { tx.result.CreatedAt = tx.result.CreatedAt.Add(time.Second) }, domain.ErrInvalidFileEntryData, 1},
		{"zero creation", func(tx *renameEntryTransactionStub) { tx.result.CreatedAt = time.Time{} }, domain.ErrInvalidFileEntryData, 1},
		{"zero modified", func(tx *renameEntryTransactionStub) { tx.result.ModifiedAt = time.Time{} }, domain.ErrInvalidFileEntryData, 1},
		{"zero updated", func(tx *renameEntryTransactionStub) { tx.result.UpdatedAt = time.Time{} }, domain.ErrInvalidFileEntryData, 1},
		{"same name changed time", func(tx *renameEntryTransactionStub) { tx.entries[13].Name, tx.result.Revision = tx.result.Name, 3 }, domain.ErrInvalidFileEntryData, 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, transaction := renameEntryServiceFixture()
			testCase.change(transaction)
			renamer := NewRenameEntryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, RenameEntryTransaction) error) error {
				return action(ctx, transaction)
			})
			entry, err := renamer.Rename(context.Background(), actor, 13, 3, "Cafe\u0301.PDF")
			if entry != nil || !errors.Is(err, testCase.want) || transaction.writeCalls != testCase.writes {
				t.Fatal("错误链、空结果或写入次数错误", err)
			}
			if errors.Is(err, domain.ErrEntryRevisionConflict) {
				var conflict *domain.EntryRevisionConflictError
				if !errors.As(err, &conflict) || conflict.CurrentRevision != 4 {
					t.Fatal("当前版本丢失")
				}
			}
		})
	}
}

func TestRenameEntryServiceInputsAndTransaction(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "zero id", "zero revision", "invalid name", "canceled input", "nil tx context", "nil store", "canceled tx", "root", "maximum changed name", "canceled profile", "canceled path", "canceled write", "canceled before commit", "commit failure", "no callback", "canceled after commit"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction := renameEntryServiceFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var requestContext context.Context = ctx
			entryID, revision, name := uint64(13), uint64(3), "Cafe\u0301.PDF"
			var want error
			switch mode {
			case "nil context":
				requestContext = nil
			case "zero user":
				actor.UserID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero session":
				actor.SessionID = 0
			case "zero id":
				entryID = 0
			case "zero revision":
				revision = 0
			case "invalid name":
				name, want = "CON.txt", domain.ErrInvalidEntryName
			case "canceled input":
				cancel()
				want = context.Canceled
			case "root":
				entryID, revision, want = 11, 1, domain.ErrRootEntryRename
			case "maximum changed name":
				revision, transaction.entries[13].Revision, want = ^uint64(0), ^uint64(0), domain.ErrEntryRevisionExhausted
			case "canceled profile":
				transaction.afterProfile, want = cancel, context.Canceled
			case "canceled path":
				transaction.afterEntry, want = func(uint64) { cancel() }, context.Canceled
			case "canceled write":
				transaction.afterWrite, transaction.writeError, want = cancel, errors.New("write failed"), context.Canceled
			case "canceled tx", "canceled before commit":
				want = context.Canceled
			case "commit failure":
				want = errors.New("fixture commit failed")
			case "no callback":
				want = domain.ErrInvalidFileEntryData
			}
			calls := 0
			renamer := NewRenameEntryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, RenameEntryTransaction) error) error {
				calls++
				switch mode {
				case "nil tx context":
					return action(nil, transaction)
				case "nil store":
					return action(ctx, nil)
				case "canceled tx":
					cancel()
				case "no callback":
					return nil
				}
				if err := action(ctx, transaction); err != nil {
					return err
				}
				switch mode {
				case "canceled before commit":
					cancel()
					return ctx.Err()
				case "commit failure":
					return want
				case "canceled after commit":
					cancel()
				}
				return nil
			})
			entry, err := renamer.Rename(requestContext, actor, entryID, revision, name)
			if mode == "canceled after commit" {
				if err != nil || entry == nil {
					t.Fatal("已确认提交的结果丢失", err)
				}
				return
			}
			if entry != nil || err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatal("输入或事务错误未传播", err)
			}
			if strings.HasPrefix(mode, "zero ") || mode == "nil context" || mode == "invalid name" || mode == "canceled input" {
				if calls != 0 {
					t.Fatal("无效输入进入事务")
				}
			}
			if (mode == "root" || mode == "maximum changed name") && transaction.writeCalls != 0 {
				t.Fatal("规则拒绝后调用了写入")
			}
		})
	}
	t.Run("missing dependency", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("缺失事务方法未拒绝")
			}
		}()
		NewRenameEntryService(nil)
	})
}
