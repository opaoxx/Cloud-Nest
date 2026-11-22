package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

type idempotentRenameTransactionStub struct {
	*entryDetailReaderStub
	writer      *renameEntryTransactionStub
	records     *idempotentDirectoryTransactionStub
	afterRename func()
}

func (transaction *idempotentRenameTransactionStub) RenameUserFileEntry(ctx context.Context, entryID, revision uint64, metadata domain.EntryNameMetadata, nameKey []byte) (*domain.FileEntry, error) {
	entry, err := transaction.writer.RenameUserFileEntry(ctx, entryID, revision, metadata, nameKey)
	if err == nil && entry != nil {
		entryCopy := *entry
		entryCopy.ParentID = copyOptionalValue(entry.ParentID)
		transaction.entries[entryID] = &entryCopy
		transaction.display.Category = copyOptionalValue(metadata.Category)
		transaction.display.Extension = copyOptionalValue(metadata.Extension)
		if transaction.afterRename != nil {
			transaction.afterRename()
		}
	}
	return entry, err
}

func (transaction *idempotentRenameTransactionStub) FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error) {
	return transaction.records.FindUserIdempotencyRecord(ctx, userID, operation, key)
}

func (transaction *idempotentRenameTransactionStub) CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error) {
	return transaction.records.CreateUserIdempotencyRecord(ctx, input)
}

func (transaction *idempotentRenameTransactionStub) AppendUserChangeEvent(ctx context.Context, input domain.ChangeEventInput) (uint64, error) {
	return transaction.records.AppendUserChangeEvent(ctx, input)
}

func encodeIdempotentRenameTestResult(detail *domain.EntryDetail) (json.RawMessage, error) {
	data, err := dto.NewRenameEntryData(detail)
	if err != nil {
		return nil, err
	}
	return json.Marshal(data)
}

func validateIdempotentRenameTestResult(body json.RawMessage) error {
	_, err := dto.RestoreRenameEntryData(body)
	return err
}

func idempotentRenameFixture() (domain.Actor, *idempotentRenameTransactionStub, *IdempotentRenameEntryService) {
	actor, reader := entryDetailFixture()
	_, writer := renameEntryServiceFixture()
	transaction := &idempotentRenameTransactionStub{
		entryDetailReaderStub: reader, writer: writer,
		records: &idempotentDirectoryTransactionStub{findError: domain.ErrIdempotencyRecordNotFound},
	}
	renamer := NewIdempotentRenameEntryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentRenameEntryTransaction) error) error {
		return action(ctx, transaction)
	}, encodeIdempotentRenameTestResult, validateIdempotentRenameTestResult)
	renamer.now = func() time.Time { return time.Date(2026, 11, 16, 1, 30, 0, 123456789, time.UTC) }
	return actor, transaction, renamer
}

func TestIdempotentRenameEntryServiceRenameAndReplay(t *testing.T) {
	for _, mode := range []string{"file", "folder", "same name"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction, renamer := idempotentRenameFixture()
			name := "Cafe\u0301.PDF"
			if mode == "folder" {
				transaction.entries[13].EntryType, transaction.entries[13].FileSize = "DIR", 0
				transaction.writer.result.EntryType, transaction.writer.result.FileSize = "DIR", 0
				transaction.display.Category = nil
			}
			if mode == "same name" {
				name = transaction.entries[13].Name
				entry := *transaction.entries[13]
				transaction.writer.result = &entry
			}
			requestContext := context.Background()
			type transactionKey struct{}
			txContext := context.WithValue(requestContext, transactionKey{}, "rename transaction")
			calls, encodeCalls := 0, 0
			var encoded json.RawMessage
			renamer.withTransaction = func(ctx context.Context, current domain.Actor, action func(context.Context, IdempotentRenameEntryTransaction) error) error {
				calls++
				if ctx != requestContext || current != actor {
					t.Error("事务入口上下文或身份错误")
				}
				return action(txContext, transaction)
			}
			renamer.encodeResult = func(detail *domain.EntryDetail) (json.RawMessage, error) {
				encodeCalls++
				var err error
				encoded, err = encodeIdempotentRenameTestResult(detail)
				return encoded, err
			}
			body, err := renamer.Rename(requestContext, actor, 13, 3, "  "+name+"  ", "request-007")
			if err != nil || body == nil || calls != 1 || transaction.writer.writeCalls != 1 || transaction.records.saveCalls != 1 || encodeCalls != 1 {
				t.Fatal("首次改名未在一次事务内完成", err)
			}
			hash, err := BuildRenameEntryRequestHash(13, 3, name)
			input := transaction.records.saveInput
			if err != nil || input.RequestHash != hash || input.Operation != domain.RenameEntryOperation || input.IdempotencyKey != "request-007" || input.ResourceType != "ENTRY" || input.ResourceID == nil || *input.ResourceID != 13 || input.ResponseStatus != 200 || !input.ExpiresAt.Equal(renamer.now().UTC().Truncate(time.Microsecond).Add(8*24*time.Hour)) {
				t.Fatal("保存参数或八天保留期错误", err)
			}
			if transaction.records.findContext != txContext || transaction.writer.writeContext != txContext || transaction.displayContext != txContext || transaction.preferenceContext != txContext || transaction.records.saveContext != txContext || transaction.records.userID != actor.UserID || transaction.records.operation != domain.RenameEntryOperation {
				t.Fatal("查询、改名、详情及保存未复用当前事务范围")
			}
			for _, ctx := range transaction.contexts {
				if ctx != txContext {
					t.Fatal("路径读取上下文错误")
				}
			}
			first, err := dto.RestoreRenameEntryData(body)
			if err != nil || first.Entry.Name != name || first.Entry.Revision != strconv.FormatUint(transaction.writer.result.Revision, 10) {
				t.Fatal("稳定改名结果错误", err)
			}
			wantEvents := 1
			if mode == "same name" {
				wantEvents = 0
			}
			if transaction.records.appendCalls != wantEvents {
				t.Fatal("实际改名与同名请求的事件数量不符")
			}
			if wantEvents == 1 {
				event := transaction.records.event
				if transaction.records.appendContext != txContext || event.EntryID != 13 || event.Revision != 4 ||
					event.EventType != "ENTRY_RENAMED" || string(event.Payload) != "{\"entry_id\":\"13\",\"parent_id\":\"12\",\"name\":\"Cafe\u0301.PDF\",\"revision\":\"4\"}" {
					t.Fatal("改名事件未记录同一事务的新名称和版本", event)
				}
			}
			transaction.records.record, transaction.records.findError = transaction.records.saved, nil
			queryCount := len(transaction.contexts)
			transaction.entries, transaction.display, transaction.preference = nil, nil, nil
			replayed, err := renamer.Rename(requestContext, actor, 13, 3, name, "request-007")
			if err != nil || !bytes.Equal(body, replayed) || calls != 2 || transaction.writer.writeCalls != 1 || transaction.records.saveCalls != 1 ||
				transaction.records.appendCalls != wantEvents || encodeCalls != 1 || len(transaction.contexts) != queryCount {
				t.Fatal("重试未复用原始快照", err)
			}
			original := bytes.Clone(transaction.records.saved.ResponseBody)
			body[0], replayed[0], encoded[0] = 'x', 'y', 'z'
			if !bytes.Equal(transaction.records.saved.ResponseBody, original) {
				t.Fatal("结果共享了可变内存")
			}
		})
	}
}

func TestIdempotentRenameEntryServiceFailures(t *testing.T) {
	failure := errors.New("fixture rename dependency")
	for _, mode := range []string{
		"query", "nil query", "record with missing error", "rename", "display", "preference", "stale detail", "encode", "validate encoded", "save", "duplicate save", "append event", "bad saved target", "bad saved body", "nil saved record",
		"conflict", "expired", "bad replay body", "transaction entry", "commit", "no callback", "nil tx context", "nil store", "zero clock", "cancel before query", "cancel after query", "cancel after rename", "cancel after display", "cancel after encode", "cancel after save", "cancel after validation",
	} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction, renamer := idempotentRenameFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := failure
			switch mode {
			case "query":
				transaction.records.findError = failure
			case "nil query":
				transaction.records.findError, want = nil, domain.ErrInvalidIdempotencyRecordData
			case "record with missing error":
				transaction.records.record, want = &domain.IdempotencyRecord{}, domain.ErrInvalidIdempotencyRecordData
			case "rename":
				transaction.writer.writeError = failure
			case "display":
				transaction.displayError = failure
			case "preference":
				transaction.preferenceError = failure
			case "stale detail":
				transaction.afterRename = func() { transaction.entries[13].Revision++ }
				want = domain.ErrInvalidFileEntryData
			case "encode":
				renamer.encodeResult = func(*domain.EntryDetail) (json.RawMessage, error) { return nil, failure }
			case "validate encoded":
				renamer.validateResult = func(json.RawMessage) error { return failure }
			case "save":
				transaction.records.saveError = failure
			case "duplicate save":
				transaction.records.saveError, want = domain.ErrIdempotencyRecordAlreadyExists, domain.ErrIdempotencyRecordAlreadyExists
			case "append event":
				transaction.records.appendError = failure
			case "bad saved target":
				transaction.records.afterSave = func(record *domain.IdempotencyRecord) { *record.ResourceID = 99 }
				want = domain.ErrInvalidIdempotencyRecordData
			case "bad saved body":
				transaction.records.afterSave = func(record *domain.IdempotencyRecord) { record.ResponseBody = json.RawMessage(`{"entry":{"id":"13"}}`) }
				want = domain.ErrInvalidIdempotencyRecordData
			case "nil saved record":
				transaction.records.afterSave = func(*domain.IdempotencyRecord) { transaction.records.saved = nil }
				want = domain.ErrInvalidIdempotencyRecordData
			case "conflict", "expired", "bad replay body":
				if _, err := renamer.Rename(ctx, actor, 13, 3, "Cafe\u0301.PDF", "request-007"); err != nil {
					t.Fatal(err)
				}
				transaction.records.record, transaction.records.findError = transaction.records.saved, nil
				switch mode {
				case "conflict":
					transaction.records.record.RequestHash[0] ^= 0xff
					want = domain.ErrIdempotencyRequestConflict
				case "expired":
					renamer.now = func() time.Time { return transaction.records.record.ExpiresAt }
					want = domain.ErrIdempotencyRecordExpired
				case "bad replay body":
					transaction.records.record.ResponseBody = json.RawMessage(`{"entry":{"id":"13"}}`)
					want = domain.ErrInvalidIdempotencyRecordData
				}
			case "transaction entry", "commit", "no callback", "nil tx context", "nil store":
				renamer.withTransaction = func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentRenameEntryTransaction) error) error {
					switch mode {
					case "transaction entry":
						return failure
					case "no callback":
						return nil
					case "nil tx context":
						return action(nil, transaction)
					case "nil store":
						return action(ctx, nil)
					}
					if err := action(ctx, transaction); err != nil {
						return err
					}
					return failure
				}
				if mode == "no callback" {
					want = domain.ErrInvalidIdempotencyRecordData
				}
				if mode == "nil tx context" || mode == "nil store" {
					want = nil
				}
			case "zero clock":
				renamer.now = func() time.Time { return time.Time{} }
				want = domain.ErrInvalidIdempotencyRecordData
			case "cancel before query":
				renamer.withTransaction = func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentRenameEntryTransaction) error) error {
					cancel()
					return action(ctx, transaction)
				}
				want = context.Canceled
			case "cancel after query":
				transaction.records.afterFind, want = cancel, context.Canceled
			case "cancel after rename":
				transaction.writer.afterWrite, want = cancel, context.Canceled
			case "cancel after display":
				transaction.afterDisplay, want = cancel, context.Canceled
			case "cancel after encode":
				renamer.encodeResult = func(detail *domain.EntryDetail) (json.RawMessage, error) {
					body, err := encodeIdempotentRenameTestResult(detail)
					cancel()
					return body, err
				}
				want = context.Canceled
			case "cancel after save":
				transaction.records.afterSave, want = func(*domain.IdempotencyRecord) { cancel() }, context.Canceled
			case "cancel after validation":
				checks := 0
				renamer.validateResult = func(body json.RawMessage) error {
					checks++
					if checks == 2 {
						cancel()
					}
					return validateIdempotentRenameTestResult(body)
				}
				want = context.Canceled
			}
			body, err := renamer.Rename(ctx, actor, 13, 3, "Cafe\u0301.PDF", "request-007")
			if body != nil || err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatal("失败结果或错误链不符合预期", err)
			}
		})
	}
}

func TestIdempotentRenameEntryServiceInputsAndDependencies(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "zero entry", "zero revision", "invalid key", "invalid name", "cancelled", "confirmed commit then cancel"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction, renamer := idempotentRenameFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			entryID, revision, name, key := uint64(13), uint64(3), "Cafe\u0301.PDF", "request-007"
			switch mode {
			case "nil context":
				callContext = nil
			case "zero user":
				actor.UserID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero session":
				actor.SessionID = 0
			case "zero entry":
				entryID = 0
			case "zero revision":
				revision = 0
			case "invalid key":
				key = "invalid key"
			case "invalid name":
				name = "CON.txt"
			case "cancelled":
				cancel()
			case "confirmed commit then cancel":
				renamer.withTransaction = func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentRenameEntryTransaction) error) error {
					if err := action(ctx, transaction); err != nil {
						return err
					}
					cancel()
					return nil
				}
			}
			body, err := renamer.Rename(callContext, actor, entryID, revision, name, key)
			if mode == "confirmed commit then cancel" {
				if err != nil || body == nil {
					t.Fatal("已确认提交结果丢失", err)
				}
				return
			}
			if err == nil || body != nil || transaction.records.findCalls != 0 {
				t.Fatal("无效输入进入事务查询", err)
			}
		})
	}
	for _, dependency := range []string{"transaction", "encode", "validate"} {
		t.Run(dependency, func(t *testing.T) {
			_, _, renamer := idempotentRenameFixture()
			withTransaction, encode, validate := renamer.withTransaction, renamer.encodeResult, renamer.validateResult
			switch dependency {
			case "transaction":
				withTransaction = nil
			case "encode":
				encode = nil
			case "validate":
				validate = nil
			}
			defer func() {
				if recover() == nil {
					t.Error("缺失装配依赖未拒绝")
				}
			}()
			NewIdempotentRenameEntryService(withTransaction, encode, validate)
		})
	}
}
