package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

type idempotentUploadTransactionStub struct {
	*createUploadTransactionStub
	record, saved            *domain.IdempotencyRecord
	findError, saveError     error
	saveInput                domain.IdempotencyResultInput
	findCalls, saveCalls     int
	findContext, saveContext context.Context
	userID                   uint64
	operation, key           string
	afterFind                func()
	afterSave                func(*domain.IdempotencyRecord)
}

func (transaction *idempotentUploadTransactionStub) FindUserIdempotencyRecord(ctx context.Context, userID uint64, operation, key string) (*domain.IdempotencyRecord, error) {
	transaction.findCalls++
	transaction.findContext, transaction.userID, transaction.operation, transaction.key = ctx, userID, operation, key
	if transaction.afterFind != nil {
		transaction.afterFind()
	}
	return transaction.record, transaction.findError
}

func (transaction *idempotentUploadTransactionStub) CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error) {
	transaction.saveCalls++
	transaction.saveContext, transaction.saveInput = ctx, input
	if transaction.saveError != nil {
		return nil, transaction.saveError
	}
	transaction.saved = &domain.IdempotencyRecord{
		ID: 31, UserID: transaction.profile.ID, Operation: input.Operation, IdempotencyKey: input.IdempotencyKey, RequestHash: input.RequestHash,
		ResourceType: input.ResourceType, ResourceID: copyOptionalValue(input.ResourceID), ResponseStatus: input.ResponseStatus, ResponseBody: bytes.Clone(input.ResponseBody), ExpiresAt: input.ExpiresAt,
		CreatedAt: time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC),
	}
	if transaction.afterSave != nil {
		transaction.afterSave(transaction.saved)
	}
	return transaction.saved, nil
}

func encodeIdempotentUploadTestResult(task *domain.UploadTask) (json.RawMessage, error) {
	data, err := dto.NewCreateUploadData(task)
	if err != nil {
		return nil, err
	}
	return json.Marshal(data)
}

func validateIdempotentUploadTestResult(body json.RawMessage) error {
	_, err := dto.RestoreCreateUploadData(body)
	return err
}

func idempotentUploadFixture() (domain.Actor, domain.CreateUploadInput, *idempotentUploadTransactionStub, *IdempotentCreateUploadService) {
	actor, input, core := createUploadFixture()
	transaction := &idempotentUploadTransactionStub{createUploadTransactionStub: core, findError: domain.ErrIdempotencyRecordNotFound}
	creator := NewIdempotentCreateUploadService("local_minio", "cloudnest",
		func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentCreateUploadTransaction) error) error {
			return action(ctx, transaction)
		},
		encodeIdempotentUploadTestResult, validateIdempotentUploadTestResult,
	)
	creator.now = func() time.Time { return time.Date(2026, 11, 17, 9, 0, 0, 123456789, time.UTC) }
	return actor, input, transaction, creator
}

func TestIdempotentCreateUploadServiceCreateAndReplay(t *testing.T) {
	for _, mode := range []string{"ordinary", "zero bytes", "single limit", "zero digest", "binary digest", "input copied before lookup"} {
		t.Run(mode, func(t *testing.T) {
			actor, input, transaction, creator := idempotentUploadFixture()
			digest := sha256.Sum256([]byte("fixture-content"))
			switch mode {
			case "zero bytes":
				input.SizeBytes = 0
			case "single limit":
				input.SizeBytes = domain.MaxSingleUploadSizeBytes
			case "zero digest":
				input.ExpectedSHA256 = new([sha256.Size]byte)
			case "binary digest", "input copied before lookup":
				input.ExpectedSHA256 = &digest
			}
			expectedHash, err := BuildCreateUploadRequestHash(input)
			if err != nil {
				t.Fatal(err)
			}
			originalDigest := digest
			if mode == "input copied before lookup" {
				transaction.afterFind = func() { digest[0] ^= 0xff }
			}
			requestContext := context.Background()
			type transactionKey struct{}
			txContext := context.WithValue(requestContext, transactionKey{}, "idempotent upload")
			transactionCalls, encodeCalls, validateCalls := 0, 0, 0
			creator.withTransaction = func(ctx context.Context, current domain.Actor, action func(context.Context, IdempotentCreateUploadTransaction) error) error {
				transactionCalls++
				if ctx != requestContext || current != actor {
					t.Error("事务入口范围错误")
				}
				return action(txContext, transaction)
			}
			creator.encodeResult = func(task *domain.UploadTask) (json.RawMessage, error) {
				encodeCalls++
				return encodeIdempotentUploadTestResult(task)
			}
			creator.validateResult = func(body json.RawMessage) error { validateCalls++; return validateIdempotentUploadTestResult(body) }
			first, err := creator.Create(requestContext, actor, input, "request-007")
			if err != nil || first == nil || transactionCalls != 1 || transaction.findCalls != 1 || transaction.saveCalls != 1 || encodeCalls != 1 || validateCalls != 2 || !reflect.DeepEqual(transaction.events, []string{"reserve", "object", "task"}) {
				t.Fatal("首次请求未完成单次创建与保存", err)
			}
			saved := transaction.saveInput
			if saved.Operation != domain.CreateUploadOperation || saved.IdempotencyKey != "request-007" || saved.RequestHash != expectedHash || saved.ResourceType != "UPLOAD_TASK" || saved.ResourceID == nil || *saved.ResourceID != transaction.taskID || saved.ResponseStatus != 201 || !saved.ExpiresAt.Equal(creator.now().UTC().Truncate(time.Microsecond).Add(8*24*time.Hour)) {
				t.Fatal("稳定结果保存范围、摘要或期限错误")
			}
			for _, ctx := range append(transaction.contexts, transaction.writeContexts...) {
				if ctx != txContext {
					t.Fatal("核心创建未复用外层事务")
				}
			}
			if transaction.findContext != txContext || transaction.saveContext != txContext || transaction.userID != actor.UserID || transaction.operation != domain.CreateUploadOperation {
				t.Fatal("幂等读写未复用事务范围")
			}
			if mode == "input copied before lookup" && *transaction.taskInput.ExpectedSHA256 != originalDigest {
				t.Fatal("进入事务后的来源变化影响创建声明")
			}
			data, err := dto.RestoreCreateUploadData(first)
			if err != nil || data.Task.ID != "29" || data.Task.RequestedName != "学习.bin" || data.Task.SizeBytes != input.SizeBytes {
				t.Fatal("首次稳定快照错误", err)
			}
			transaction.record, transaction.findError = transaction.saved, nil
			transaction.entries = nil
			beforeReads := len(transaction.contexts)
			// 恢复调用方原声明，确认再次进入只走已存结果。
			if mode == "input copied before lookup" {
				digest = originalDigest
				transaction.afterFind = nil
			}
			second, err := creator.Create(requestContext, actor, input, "request-007")
			if err != nil || !bytes.Equal(first, second) || transactionCalls != 2 || transaction.findCalls != 2 || transaction.saveCalls != 1 || encodeCalls != 1 || validateCalls != 3 || len(transaction.events) != 3 || len(transaction.contexts) != beforeReads {
				t.Fatal("重试未复用原始结果", err)
			}
			first[0], second[0] = 'x', 'y'
			if transaction.saved.ResponseBody[0] != '{' || transaction.saveInput.ResponseBody[0] != '{' {
				t.Fatal("返回结果共享了持久正文内存")
			}
		})
	}
}

func TestIdempotentCreateUploadServiceFailures(t *testing.T) {
	failure := errors.New("fixture idempotent upload failure")
	for _, mode := range []string{"query", "nil query result", "record with missing error", "reserve", "object", "task", "encode", "validate encoded", "validate saved", "save", "duplicate save", "bad saved resource", "bad saved task ID", "nil saved result", "zero clock", "encoder changes task ID", "replay conflict", "replay expired", "replay invalid snapshot"} {
		t.Run(mode, func(t *testing.T) {
			actor, input, transaction, creator := idempotentUploadFixture()
			want := failure
			if strings.HasPrefix(mode, "replay ") {
				if _, err := creator.Create(context.Background(), actor, input, "request-007"); err != nil {
					t.Fatal(err)
				}
				transaction.record, transaction.findError = transaction.saved, nil
				switch mode {
				case "replay conflict":
					input.SizeBytes++
					want = domain.ErrIdempotencyRequestConflict
				case "replay expired":
					transaction.record.ExpiresAt = creator.now()
					want = domain.ErrIdempotencyRecordExpired
				case "replay invalid snapshot":
					transaction.record.ResponseBody = bytes.Replace(transaction.record.ResponseBody, []byte("UPLOADING"), []byte("VERIFYING"), 1)
					want = domain.ErrInvalidIdempotencyRecordData
				}
			}
			switch mode {
			case "query":
				transaction.findError = failure
			case "nil query result":
				transaction.findError = nil
				want = domain.ErrInvalidIdempotencyRecordData
			case "record with missing error":
				transaction.record = &domain.IdempotencyRecord{}
				want = domain.ErrInvalidIdempotencyRecordData
			case "reserve":
				transaction.reserveError = failure
			case "object":
				transaction.objectError = failure
			case "task":
				transaction.taskError = failure
			case "encode":
				creator.encodeResult = func(*domain.UploadTask) (json.RawMessage, error) { return nil, failure }
			case "validate encoded":
				creator.validateResult = func(json.RawMessage) error { return failure }
			case "validate saved":
				calls := 0
				creator.validateResult = func(body json.RawMessage) error {
					calls++
					if calls == 2 {
						return failure
					}
					return validateIdempotentUploadTestResult(body)
				}
			case "save":
				transaction.saveError = failure
			case "duplicate save":
				transaction.saveError = domain.ErrIdempotencyRecordAlreadyExists
				want = domain.ErrIdempotencyRecordAlreadyExists
			case "bad saved resource":
				transaction.afterSave = func(record *domain.IdempotencyRecord) { record.ResourceType = "ENTRY" }
				want = domain.ErrInvalidIdempotencyRecordData
			case "bad saved task ID":
				transaction.afterSave = func(record *domain.IdempotencyRecord) { *record.ResourceID++ }
				want = domain.ErrInvalidIdempotencyRecordData
			case "nil saved result":
				transaction.afterSave = func(*domain.IdempotencyRecord) { transaction.saved = nil }
				want = domain.ErrInvalidIdempotencyRecordData
			case "zero clock":
				creator.now = func() time.Time { return time.Time{} }
				want = domain.ErrInvalidIdempotencyRecordData
			case "encoder changes task ID":
				creator.encodeResult = func(task *domain.UploadTask) (json.RawMessage, error) {
					task.ID++
					return encodeIdempotentUploadTestResult(task)
				}
				want = domain.ErrInvalidIdempotencyRecordData
			}
			beforeWrites := len(transaction.events)
			body, err := creator.Create(context.Background(), actor, input, "request-007")
			if body != nil || !errors.Is(err, want) {
				t.Fatal("失败未返回空结果及正确原因", err)
			}
			if strings.HasPrefix(mode, "replay ") && len(transaction.events) != beforeWrites {
				t.Fatal("复用失败后又进入核心创建")
			}
			if (mode == "query" || mode == "nil query result" || mode == "record with missing error") && len(transaction.events) != 0 {
				t.Fatal("查询失败后发生了创建")
			}
		})
	}
}

func TestIdempotentCreateUploadServiceBoundary(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "bad key", "zero parent", "bad name", "oversize", "cancelled", "transaction entry", "commit failure", "missing callback", "nil tx context", "nil tx store"} {
		t.Run(mode, func(t *testing.T) {
			actor, input, transaction, creator := idempotentUploadFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("fixture transaction failure")
			calls := 0
			creator.withTransaction = func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentCreateUploadTransaction) error) error {
				calls++
				switch mode {
				case "transaction entry":
					return failure
				case "missing callback":
					return nil
				case "nil tx context":
					return action(nil, transaction)
				case "nil tx store":
					return action(ctx, nil)
				}
				if err := action(ctx, transaction); err != nil {
					return err
				}
				if mode == "commit failure" {
					return failure
				}
				return nil
			}
			key := "request-007"
			switch mode {
			case "zero user":
				actor.UserID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero session":
				actor.SessionID = 0
			case "bad key":
				key = "private key"
			case "zero parent":
				input.ParentID = 0
			case "bad name":
				input.Name = "private/name"
			case "oversize":
				input.SizeBytes = domain.MaxSingleUploadSizeBytes + 1
			case "cancelled":
				cancel()
			}
			var requestContext context.Context = ctx
			if mode == "nil context" {
				requestContext = nil
			}
			body, err := creator.Create(requestContext, actor, input, key)
			if body != nil || err == nil {
				t.Fatal("非法边界返回了成功结果")
			}
			switch mode {
			case "nil context", "zero user", "zero device", "zero session", "bad key", "zero parent", "bad name", "oversize", "cancelled":
				if calls != 0 || transaction.findCalls != 0 {
					t.Fatal("无效输入进入了事务")
				}
			case "transaction entry", "commit failure":
				if !errors.Is(err, failure) {
					t.Fatal("事务失败原因未保留", err)
				}
			}
		})
	}
}

func TestIdempotentCreateUploadServiceCancellation(t *testing.T) {
	for _, phase := range []string{"before lookup", "lookup", "reserve", "object", "task", "encode", "validate encoded", "save", "validate saved", "after callback"} {
		t.Run(phase, func(t *testing.T) {
			actor, input, transaction, creator := idempotentUploadFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("fixture concurrent failure")
			creator.withTransaction = func(ctx context.Context, _ domain.Actor, action func(context.Context, IdempotentCreateUploadTransaction) error) error {
				if phase == "before lookup" {
					cancel()
				}
				err := action(ctx, transaction)
				if phase == "after callback" {
					cancel()
				}
				return err
			}
			switch phase {
			case "lookup":
				transaction.afterFind = cancel
				transaction.findError = failure
			case "reserve":
				transaction.afterReserve = cancel
				transaction.reserveError = failure
			case "object":
				transaction.afterObject = func(*domain.StoredObject) { cancel() }
				transaction.objectError = failure
			case "task":
				transaction.afterTask = func(*domain.UploadTask) { cancel() }
				transaction.taskError = failure
			case "encode":
				creator.encodeResult = func(*domain.UploadTask) (json.RawMessage, error) { cancel(); return nil, failure }
			case "validate encoded", "validate saved":
				calls := 0
				creator.validateResult = func(body json.RawMessage) error {
					calls++
					if (phase == "validate encoded" && calls == 1) || (phase == "validate saved" && calls == 2) {
						cancel()
						return failure
					}
					return validateIdempotentUploadTestResult(body)
				}
			case "save":
				transaction.afterSave = func(*domain.IdempotencyRecord) { cancel() }
			}
			body, err := creator.Create(ctx, actor, input, "request-007")
			if body != nil || !errors.Is(err, context.Canceled) {
				t.Fatal("取消未优先返回上下文错误", err)
			}
		})
	}
}

func TestNewIdempotentCreateUploadServiceDependencies(t *testing.T) {
	for _, mode := range []string{"transaction", "encoder", "validator", "provider", "bucket", "long bucket", "non ASCII provider"} {
		t.Run(mode, func(t *testing.T) {
			withTransaction := func(context.Context, domain.Actor, func(context.Context, IdempotentCreateUploadTransaction) error) error {
				return nil
			}
			encoder, validator := encodeIdempotentUploadTestResult, validateIdempotentUploadTestResult
			provider, bucket := "local_minio", "cloudnest"
			switch mode {
			case "transaction":
				withTransaction = nil
			case "encoder":
				encoder = nil
			case "validator":
				validator = nil
			case "provider":
				provider = ""
			case "bucket":
				bucket = ""
			case "long bucket":
				bucket = strings.Repeat("a", 64)
			case "non ASCII provider":
				provider = "存储"
			}
			defer func() {
				if recover() == nil {
					t.Fatal("缺少装配依赖应触发panic")
				}
			}()
			NewIdempotentCreateUploadService(provider, bucket, withTransaction, encoder, validator)
		})
	}
}
