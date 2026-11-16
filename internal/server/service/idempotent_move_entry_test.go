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

type idempotentMoveTransactionStub struct {
	*entryDetailReaderStub
	writer    *moveEntryTransactionStub
	records   *idempotentDirectoryTransactionStub
	afterMove func()
}

func (transaction *idempotentMoveTransactionStub) FindUserEntrySubtreeStats(ctx context.Context, userID, entryID uint64) (*domain.EntrySubtreeStats, error) {
	return transaction.writer.FindUserEntrySubtreeStats(ctx, userID, entryID)
}

func (transaction *idempotentMoveTransactionStub) MoveUserFileEntry(ctx context.Context, entryID, revision, targetParentID uint64) (*domain.FileEntry, error) {
	entry, err := transaction.writer.MoveUserFileEntry(ctx, entryID, revision, targetParentID)
	if err == nil && entry != nil {
		entryCopy := *entry
		entryCopy.ParentID = copyOptionalValue(entry.ParentID)
		transaction.entries[entryID] = &entryCopy
		if transaction.afterMove != nil {
			transaction.afterMove()
		}
	}
	return entry, err
}

func (transaction *idempotentMoveTransactionStub) FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error) {
	return transaction.records.FindUserIdempotencyRecord(ctx, userID, operation, key)
}

func (transaction *idempotentMoveTransactionStub) CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error) {
	return transaction.records.CreateUserIdempotencyRecord(ctx, input)
}

func encodeIdempotentMoveTestResult(detail *domain.EntryDetail) (json.RawMessage, error) {
	data, err := dto.NewMoveEntryData(detail)
	if err != nil {
		return nil, err
	}
	return json.Marshal(data)
}

func validateIdempotentMoveTestResult(body json.RawMessage) error {
	_, err := dto.RestoreMoveEntryData(body)
	return err
}

func idempotentMoveFixture() (domain.Actor, *idempotentMoveTransactionStub, *IdempotentMoveEntryService) {
	actor, reader := entryDetailFixture()
	_, writer := moveEntryServiceFixture()
	reader.entries[14] = writer.entries[14]
	transaction := &idempotentMoveTransactionStub{
		entryDetailReaderStub: reader, writer: writer,
		records: &idempotentDirectoryTransactionStub{findError: domain.ErrIdempotencyRecordNotFound},
	}
	mover := NewIdempotentMoveEntryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentMoveEntryTransaction) error) error {
		return action(ctx, transaction)
	}, encodeIdempotentMoveTestResult, validateIdempotentMoveTestResult)
	mover.now = func() time.Time { return time.Date(2026, 11, 16, 1, 30, 0, 123456789, time.UTC) }
	return actor, transaction, mover
}

func TestIdempotentMoveEntryServiceMoveAndReplay(t *testing.T) {
	for _, mode := range []string{"file", "folder", "same position"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction, mover := idempotentMoveFixture()
			targetID := uint64(14)
			if mode == "folder" {
				transaction.entries[13].EntryType, transaction.entries[13].FileSize = "DIR", 0
				transaction.writer.result.EntryType, transaction.writer.result.FileSize = "DIR", 0
				transaction.display.Category, transaction.display.Extension = nil, nil
			}
			if mode == "same position" {
				targetID = *transaction.entries[13].ParentID
				entry := *transaction.entries[13]
				transaction.writer.result = &entry
			}
			requestContext := context.Background()
			type transactionKey struct{}
			txContext := context.WithValue(requestContext, transactionKey{}, "move transaction")
			calls, encodeCalls := 0, 0
			var encoded json.RawMessage
			mover.withTransaction = func(ctx context.Context, current domain.Actor, action func(context.Context, IdempotentMoveEntryTransaction) error) error {
				calls++
				if ctx != requestContext || current != actor {
					t.Error("事务入口上下文或身份错误")
				}
				return action(txContext, transaction)
			}
			mover.encodeResult = func(detail *domain.EntryDetail) (json.RawMessage, error) {
				encodeCalls++
				var err error
				encoded, err = encodeIdempotentMoveTestResult(detail)
				return encoded, err
			}
			body, err := mover.Move(requestContext, actor, 13, 3, targetID, "request-007")
			if err != nil || body == nil || calls != 1 || transaction.writer.writeCalls != 1 || transaction.records.saveCalls != 1 || encodeCalls != 1 {
				t.Fatal("首次移动未在一次事务内完成", err)
			}
			hash, err := BuildMoveEntryRequestHash(13, 3, targetID)
			input := transaction.records.saveInput
			if err != nil || input.RequestHash != hash || input.Operation != domain.MoveEntryOperation || input.IdempotencyKey != "request-007" || input.ResourceType != "ENTRY" || input.ResourceID == nil || *input.ResourceID != 13 || input.ResponseStatus != 200 || !input.ExpiresAt.Equal(mover.now().UTC().Truncate(time.Microsecond).Add(8*24*time.Hour)) {
				t.Fatal("保存参数或八天保留期错误", err)
			}
			if transaction.records.findContext != txContext || transaction.writer.writeContext != txContext || transaction.displayContext != txContext || transaction.preferenceContext != txContext || transaction.records.saveContext != txContext || transaction.records.userID != actor.UserID || transaction.records.operation != domain.MoveEntryOperation {
				t.Fatal("查询、移动、详情及保存未复用当前事务范围")
			}
			for _, ctx := range transaction.contexts {
				if ctx != txContext {
					t.Fatal("路径读取上下文错误")
				}
			}
			first, err := dto.RestoreMoveEntryData(body)
			if err != nil || first.Entry.ParentID == nil || *first.Entry.ParentID != strconv.FormatUint(targetID, 10) || first.Entry.Revision != strconv.FormatUint(transaction.writer.result.Revision, 10) {
				t.Fatal("稳定移动结果错误", err)
			}
			transaction.records.record, transaction.records.findError = transaction.records.saved, nil
			queryCount := len(transaction.contexts)
			transaction.entries, transaction.display, transaction.preference = nil, nil, nil
			replayed, err := mover.Move(requestContext, actor, 13, 3, targetID, "request-007")
			if err != nil || !bytes.Equal(body, replayed) || calls != 2 || transaction.writer.writeCalls != 1 || transaction.records.saveCalls != 1 || encodeCalls != 1 || len(transaction.contexts) != queryCount {
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

func TestIdempotentMoveEntryServiceFailures(t *testing.T) {
	failure := errors.New("fixture move dependency")
	for _, mode := range []string{
		"query", "nil query", "record with missing error", "move", "stats", "display", "preference", "stale detail", "encode", "validate encoded", "save", "duplicate save", "bad saved target", "bad saved parent", "bad saved body", "nil saved record",
		"conflict", "expired", "bad replay body", "bad replay parent", "transaction entry", "commit", "no callback", "nil tx context", "nil store", "zero clock", "cancel before query", "cancel after query", "cancel after move", "cancel after display", "cancel after encode", "cancel after save", "cancel after validation",
	} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction, mover := idempotentMoveFixture()
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
			case "move":
				transaction.writer.writeError = failure
			case "stats":
				transaction.writer.statsError = failure
			case "display":
				transaction.displayError = failure
			case "preference":
				transaction.preferenceError = failure
			case "stale detail":
				transaction.afterMove = func() { transaction.entries[13].Revision++ }
				want = domain.ErrInvalidFileEntryData
			case "encode":
				mover.encodeResult = func(*domain.EntryDetail) (json.RawMessage, error) { return nil, failure }
			case "validate encoded":
				mover.validateResult = func(json.RawMessage) error { return failure }
			case "save":
				transaction.records.saveError = failure
			case "duplicate save":
				transaction.records.saveError, want = domain.ErrIdempotencyRecordAlreadyExists, domain.ErrIdempotencyRecordAlreadyExists
			case "bad saved target":
				transaction.records.afterSave = func(record *domain.IdempotencyRecord) { *record.ResourceID = 99 }
				want = domain.ErrInvalidIdempotencyRecordData
			case "bad saved parent":
				transaction.records.afterSave = func(record *domain.IdempotencyRecord) {
					record.ResponseBody = bytes.Replace(record.ResponseBody, []byte(`"parent_id":"14"`), []byte(`"parent_id":"11"`), 1)
				}
				want = domain.ErrInvalidIdempotencyRecordData
			case "bad saved body":
				transaction.records.afterSave = func(record *domain.IdempotencyRecord) {
					record.ResponseBody = json.RawMessage(`{"entry":{"id":"13","parent_id":"14"}}`)
				}
				want = domain.ErrInvalidIdempotencyRecordData
			case "nil saved record":
				transaction.records.afterSave = func(*domain.IdempotencyRecord) { transaction.records.saved = nil }
				want = domain.ErrInvalidIdempotencyRecordData
			case "conflict", "expired", "bad replay body", "bad replay parent":
				if _, err := mover.Move(ctx, actor, 13, 3, 14, "request-007"); err != nil {
					t.Fatal(err)
				}
				transaction.records.record, transaction.records.findError = transaction.records.saved, nil
				switch mode {
				case "conflict":
					transaction.records.record.RequestHash[0] ^= 0xff
					want = domain.ErrIdempotencyRequestConflict
				case "expired":
					mover.now = func() time.Time { return transaction.records.record.ExpiresAt }
					want = domain.ErrIdempotencyRecordExpired
				case "bad replay body":
					transaction.records.record.ResponseBody = json.RawMessage(`{"entry":{"id":"13","parent_id":"14"}}`)
					want = domain.ErrInvalidIdempotencyRecordData
				case "bad replay parent":
					transaction.records.record.ResponseBody = bytes.Replace(transaction.records.record.ResponseBody, []byte(`"parent_id":"14"`), []byte(`"parent_id":"11"`), 1)
					want = domain.ErrInvalidIdempotencyRecordData
				}
			case "transaction entry", "commit", "no callback", "nil tx context", "nil store":
				mover.withTransaction = func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentMoveEntryTransaction) error) error {
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
				mover.now = func() time.Time { return time.Time{} }
				want = domain.ErrInvalidIdempotencyRecordData
			case "cancel before query":
				mover.withTransaction = func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentMoveEntryTransaction) error) error {
					cancel()
					return action(ctx, transaction)
				}
				want = context.Canceled
			case "cancel after query":
				transaction.records.afterFind, want = cancel, context.Canceled
			case "cancel after move":
				transaction.writer.afterWrite, want = cancel, context.Canceled
			case "cancel after display":
				transaction.afterDisplay, want = cancel, context.Canceled
			case "cancel after encode":
				mover.encodeResult = func(detail *domain.EntryDetail) (json.RawMessage, error) {
					body, err := encodeIdempotentMoveTestResult(detail)
					cancel()
					return body, err
				}
				want = context.Canceled
			case "cancel after save":
				transaction.records.afterSave, want = func(*domain.IdempotencyRecord) { cancel() }, context.Canceled
			case "cancel after validation":
				checks := 0
				mover.validateResult = func(body json.RawMessage) error {
					checks++
					if checks == 2 {
						cancel()
					}
					return validateIdempotentMoveTestResult(body)
				}
				want = context.Canceled
			}
			body, err := mover.Move(ctx, actor, 13, 3, 14, "request-007")
			if body != nil || err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatal("失败结果或错误链不符合预期", err)
			}
		})
	}
}

func TestIdempotentMoveEntryServiceInputsAndDependencies(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "zero entry", "zero revision", "invalid key", "zero destination", "cancelled", "confirmed commit then cancel"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction, mover := idempotentMoveFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			entryID, revision, targetID, key := uint64(13), uint64(3), uint64(14), "request-007"
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
			case "zero destination":
				targetID = 0
			case "cancelled":
				cancel()
			case "confirmed commit then cancel":
				mover.withTransaction = func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentMoveEntryTransaction) error) error {
					if err := action(ctx, transaction); err != nil {
						return err
					}
					cancel()
					return nil
				}
			}
			body, err := mover.Move(callContext, actor, entryID, revision, targetID, key)
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
			_, _, mover := idempotentMoveFixture()
			withTransaction, encode, validate := mover.withTransaction, mover.encodeResult, mover.validateResult
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
			NewIdempotentMoveEntryService(withTransaction, encode, validate)
		})
	}
}
