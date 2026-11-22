package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

type idempotentDirectoryTransactionStub struct {
	*createDirectoryTransactionStub
	record         *domain.IdempotencyRecord
	findError      error
	saveError      error
	appendError    error
	saved          *domain.IdempotencyRecord
	saveInput      domain.IdempotencyResultInput
	findCalls      int
	saveCalls      int
	appendCalls    int
	findContext    context.Context
	saveContext    context.Context
	appendContext  context.Context
	event          domain.ChangeEventInput
	userID         uint64
	operation, key string
	afterFind      func()
	afterSave      func(*domain.IdempotencyRecord)
}

func (transaction *idempotentDirectoryTransactionStub) AppendUserChangeEvent(ctx context.Context, input domain.ChangeEventInput) (uint64, error) {
	transaction.appendCalls++
	transaction.appendContext, transaction.event = ctx, input
	if transaction.appendError != nil {
		return 0, transaction.appendError
	}
	return 1, nil
}

func (transaction *idempotentDirectoryTransactionStub) FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error) {
	transaction.findCalls++
	transaction.findContext, transaction.userID, transaction.operation, transaction.key = ctx, userID, operation, key
	if transaction.afterFind != nil {
		transaction.afterFind()
	}
	return transaction.record, transaction.findError
}

func (transaction *idempotentDirectoryTransactionStub) CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error) {
	transaction.saveCalls++
	transaction.saveContext, transaction.saveInput = ctx, input
	if transaction.saveError != nil {
		return nil, transaction.saveError
	}
	transaction.saved = &domain.IdempotencyRecord{
		ID: 31, UserID: 7, Operation: input.Operation, IdempotencyKey: input.IdempotencyKey, RequestHash: input.RequestHash,
		ResourceType: input.ResourceType, ResourceID: copyOptionalValue(input.ResourceID), ResponseStatus: input.ResponseStatus,
		ResponseBody: bytes.Clone(input.ResponseBody), ExpiresAt: input.ExpiresAt,
		CreatedAt: time.Date(2026, 11, 16, 1, 30, 0, 123456000, time.UTC),
	}
	if transaction.afterSave != nil {
		transaction.afterSave(transaction.saved)
	}
	return transaction.saved, nil
}

func encodeIdempotentDirectoryTestResult(entry *domain.FileEntry, color *string) (json.RawMessage, error) {
	data, err := dto.NewCreateDirectoryData(entry, color)
	if err != nil {
		return nil, err
	}
	return json.Marshal(data)
}

func validateIdempotentDirectoryTestResult(body json.RawMessage) error {
	_, err := dto.RestoreCreateDirectoryData(body)
	return err
}

func idempotentDirectoryFixture() (domain.Actor, *idempotentDirectoryTransactionStub, *IdempotentCreateDirectoryService) {
	actor, core := createDirectoryFixture()
	transaction := &idempotentDirectoryTransactionStub{createDirectoryTransactionStub: core, findError: domain.ErrIdempotencyRecordNotFound}
	creator := NewIdempotentCreateDirectoryService(
		func(ctx context.Context, actor domain.Actor, action func(context.Context, IdempotentCreateDirectoryTransaction) error) error {
			return action(ctx, transaction)
		}, encodeIdempotentDirectoryTestResult, validateIdempotentDirectoryTestResult,
	)
	creator.now = func() time.Time { return time.Date(2026, 11, 16, 1, 30, 0, 123456789, time.UTC) }
	return actor, transaction, creator
}

func TestIdempotentCreateDirectoryServiceCreateAndReplay(t *testing.T) {
	empty, green := "", "green"
	for _, color := range []*string{nil, &empty, &green} {
		actor, transaction, creator := idempotentDirectoryFixture()
		requestContext := context.Background()
		type transactionKey struct{}
		txContext := context.WithValue(requestContext, transactionKey{}, "user transaction")
		transactionCalls, encodeCalls := 0, 0
		creator.withTransaction = func(ctx context.Context, current domain.Actor, action func(context.Context, IdempotentCreateDirectoryTransaction) error) error {
			transactionCalls++
			if ctx != requestContext || current != actor {
				t.Error("事务身份或上下文错误")
			}
			return action(txContext, transaction)
		}
		creator.encodeResult = func(entry *domain.FileEntry, actualColor *string) (json.RawMessage, error) {
			encodeCalls++
			return encodeIdempotentDirectoryTestResult(entry, actualColor)
		}
		body, err := creator.Create(requestContext, actor, 12, "  Cafe\u0301  ", color, "request-007")
		if err != nil || body == nil || transactionCalls != 1 || transaction.createCalls != 1 || transaction.saveCalls != 1 || transaction.appendCalls != 1 || encodeCalls != 1 {
			t.Fatal("首次创建未在一次事务内完成", err)
		}
		hash, err := BuildCreateDirectoryRequestHash(12, "Cafe\u0301", color)
		if err != nil || transaction.saveInput.RequestHash != hash || transaction.saveInput.Operation != domain.CreateDirectoryOperation || transaction.saveInput.IdempotencyKey != "request-007" ||
			transaction.saveInput.ResourceType != "ENTRY" || *transaction.saveInput.ResourceID != 23 || transaction.saveInput.ResponseStatus != 201 ||
			!transaction.saved.ExpiresAt.Equal(creator.now().UTC().Truncate(time.Microsecond).Add(8*24*time.Hour)) {
			t.Fatal("幂等保存参数、规范化摘要或八天保留期错误", err)
		}
		if transaction.findContext != txContext || transaction.createContext != txContext || transaction.saveContext != txContext ||
			transaction.appendContext != txContext || transaction.userID != actor.UserID || transaction.operation != domain.CreateDirectoryOperation || transaction.key != "request-007" {
			t.Fatal("查询、创建及保存未复用事务上下文或范围")
		}
		if event := transaction.event; event.EntryID != 23 || event.Revision != 1 || event.EventType != "DIRECTORY_CREATED" ||
			string(event.Payload) != "{\"entry_id\":\"23\",\"parent_id\":\"12\",\"name\":\"Cafe\u0301\",\"revision\":\"1\"}" {
			t.Fatal("新建目录事件未采用已确认目录的标识、展示名和版本", event)
		}
		first, err := dto.RestoreCreateDirectoryData(body)
		if err != nil || first.Entry.Name != "Cafe\u0301" || !reflect.DeepEqual(first.Entry.ColorKey, color) {
			t.Fatal("稳定结果未保留展示名或颜色", err)
		}
		transaction.record, transaction.findError = transaction.saved, nil
		transaction.entries = nil
		queryCount := len(transaction.contexts)
		replayed, err := creator.Create(requestContext, actor, 12, "Cafe\u0301", color, "request-007")
		if err != nil || !bytes.Equal(body, replayed) || transactionCalls != 2 || transaction.createCalls != 1 || transaction.saveCalls != 1 ||
			transaction.appendCalls != 1 || encodeCalls != 1 || len(transaction.contexts) != queryCount {
			t.Fatal("重试未复用原始快照或重复进入创建流程", err)
		}
		body[0], replayed[0] = 'x', 'y'
		if transaction.saved.ResponseBody[0] != '{' {
			t.Fatal("返回正文与记录共享可变内存")
		}
	}
}

func TestIdempotentCreateDirectoryServiceFailures(t *testing.T) {
	failure := errors.New("fixture dependency failure")
	for _, mode := range []string{
		"query", "nil query result", "record with missing error", "create", "encode", "validate encoded", "save", "duplicate save", "append event",
		"bad saved resource", "bad saved body", "nil saved result", "conflict", "expired", "bad replay snapshot",
		"transaction entry", "commit", "missing callback", "nil transaction context", "nil transaction",
		"cancel before query", "cancel after query", "cancel after create", "cancel after encode", "cancel after save", "cancel after validation",
	} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction, creator := idempotentDirectoryFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := failure
			switch mode {
			case "query":
				transaction.findError = failure
			case "nil query result":
				transaction.findError, want = nil, domain.ErrInvalidIdempotencyRecordData
			case "record with missing error":
				transaction.record, want = &domain.IdempotencyRecord{}, domain.ErrInvalidIdempotencyRecordData
			case "create":
				transaction.createError = failure
			case "encode":
				creator.encodeResult = func(*domain.FileEntry, *string) (json.RawMessage, error) { return nil, failure }
			case "validate encoded":
				creator.validateResult = func(json.RawMessage) error { return failure }
			case "save":
				transaction.saveError = failure
			case "duplicate save":
				transaction.saveError, want = domain.ErrIdempotencyRecordAlreadyExists, domain.ErrIdempotencyRecordAlreadyExists
			case "append event":
				transaction.appendError = failure
			case "bad saved resource":
				transaction.afterSave = func(record *domain.IdempotencyRecord) { *record.ResourceID = 99 }
				want = domain.ErrInvalidIdempotencyRecordData
			case "bad saved body":
				transaction.afterSave = func(record *domain.IdempotencyRecord) { record.ResponseBody = json.RawMessage(`{"entry":{"id":"23"}}`) }
				want = domain.ErrInvalidIdempotencyRecordData
			case "nil saved result":
				transaction.afterSave = func(*domain.IdempotencyRecord) { transaction.saved = nil }
				want = domain.ErrInvalidIdempotencyRecordData
			case "conflict", "expired", "bad replay snapshot":
				if _, err := creator.Create(ctx, actor, 12, "Cafe\u0301", nil, "request-007"); err != nil {
					t.Fatal(err)
				}
				transaction.record, transaction.findError = transaction.saved, nil
				switch mode {
				case "conflict":
					transaction.record.RequestHash[0] ^= 0xff
					want = domain.ErrIdempotencyRequestConflict
				case "expired":
					creator.now = func() time.Time { return transaction.record.ExpiresAt }
					want = domain.ErrIdempotencyRecordExpired
				case "bad replay snapshot":
					transaction.record.ResponseBody = json.RawMessage(`{"entry":{"id":"23"}}`)
					want = domain.ErrInvalidIdempotencyRecordData
				}
			case "transaction entry", "commit", "missing callback", "nil transaction context", "nil transaction":
				creator.withTransaction = func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentCreateDirectoryTransaction) error) error {
					switch mode {
					case "transaction entry":
						return failure
					case "missing callback":
						return nil
					case "nil transaction context":
						return action(nil, transaction)
					case "nil transaction":
						return action(ctx, nil)
					}
					if err := action(ctx, transaction); err != nil {
						return err
					}
					return failure
				}
				if mode == "missing callback" {
					want = domain.ErrInvalidIdempotencyRecordData
				}
				if mode == "nil transaction context" || mode == "nil transaction" {
					want = nil
				}
			case "cancel before query":
				creator.withTransaction = func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentCreateDirectoryTransaction) error) error {
					cancel()
					return action(ctx, transaction)
				}
				want = context.Canceled
			case "cancel after query":
				transaction.afterFind, want = cancel, context.Canceled
			case "cancel after create":
				transaction.afterCreate, want = cancel, context.Canceled
			case "cancel after encode":
				creator.encodeResult = func(entry *domain.FileEntry, color *string) (json.RawMessage, error) {
					body, err := encodeIdempotentDirectoryTestResult(entry, color)
					cancel()
					return body, err
				}
				want = context.Canceled
			case "cancel after save":
				transaction.afterSave = func(*domain.IdempotencyRecord) { cancel() }
				want = context.Canceled
			case "cancel after validation":
				checks := 0
				creator.validateResult = func(body json.RawMessage) error {
					checks++
					if checks == 2 {
						cancel()
					}
					return validateIdempotentDirectoryTestResult(body)
				}
				want = context.Canceled
			}
			body, err := creator.Create(ctx, actor, 12, "Cafe\u0301", nil, "request-007")
			if body != nil || err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatal("失败未返回空正文及预期原因", err)
			}
		})
	}
}

func TestIdempotentCreateDirectoryServiceInputsAndCommit(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "zero parent", "invalid name", "invalid color", "invalid key", "cancelled", "zero clock"} {
		t.Run(mode, func(t *testing.T) {
			actor, transaction, creator := idempotentDirectoryFixture()
			var ctx context.Context = context.Background()
			parentID, name, key := uint64(12), "Cafe\u0301", "request-007"
			var color *string
			switch mode {
			case "nil context":
				ctx = nil
			case "zero user":
				actor.UserID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero session":
				actor.SessionID = 0
			case "zero parent":
				parentID = 0
			case "invalid name":
				name = "CON.txt"
			case "invalid color":
				value := "绿色"
				color = &value
			case "invalid key":
				key = "two keys"
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			case "zero clock":
				creator.now = func() time.Time { return time.Time{} }
			}
			body, err := creator.Create(ctx, actor, parentID, name, color, key)
			if body != nil || err == nil || transaction.saveCalls != 0 || (mode != "zero clock" && transaction.findCalls != 0) {
				t.Fatal("非法输入未按预期拒绝", err)
			}
		})
	}
	actor, transaction, creator := idempotentDirectoryFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	creator.withTransaction = func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentCreateDirectoryTransaction) error) error {
		if err := action(ctx, transaction); err != nil {
			return err
		}
		cancel()
		return nil
	}
	body, err := creator.Create(ctx, actor, 12, "Cafe\u0301", nil, "request-007")
	if err != nil || len(body) == 0 || ctx.Err() == nil {
		t.Fatal("确认提交后未保留已提交结果", err)
	}
}

func TestIdempotentCreateDirectoryServiceDependencies(t *testing.T) {
	for _, missing := range []string{"transaction", "encoder", "validator"} {
		t.Run(missing, func(t *testing.T) {
			_, _, creator := idempotentDirectoryFixture()
			if missing == "transaction" {
				creator.withTransaction = nil
			}
			if missing == "encoder" {
				creator.encodeResult = nil
			}
			if missing == "validator" {
				creator.validateResult = nil
			}
			defer func() {
				if recover() == nil {
					t.Error("缺少装配依赖未拒绝")
				}
			}()
			NewIdempotentCreateDirectoryService(creator.withTransaction, creator.encodeResult, creator.validateResult)
		})
	}
}

func TestIdempotentCreateDirectoryServiceOwnedInputs(t *testing.T) {
	actor, transaction, creator := idempotentDirectoryFixture()
	color := "green"
	creator.withTransaction = func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentCreateDirectoryTransaction) error) error {
		// 调用方在进入事务后改动原颜色，服务仍使用进入事务前的独立输入。
		color = "sand"
		if err := action(ctx, transaction); err != nil {
			return err
		}
		// 模拟事务返回前原始读取缓冲区失效。
		transaction.saved.ResponseBody[0] = 'x'
		return nil
	}
	body, err := creator.Create(context.Background(), actor, 12, "Cafe\u0301", &color, "owned-input")
	if err != nil {
		t.Fatal(err)
	}
	data, err := dto.RestoreCreateDirectoryData(body)
	if err != nil || data.Entry.ColorKey == nil || *data.Entry.ColorKey != "green" || *transaction.colorKey != "green" || color != "sand" {
		t.Fatal("服务未持有独立输入及稳定输出", err)
	}
}
