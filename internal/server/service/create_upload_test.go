package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type createUploadTransactionStub struct {
	*fileEntryPathReaderStub
	events                               []string
	writeContexts                        []context.Context
	reserved                             uint64
	objectInput                          domain.CreateStagedObjectInput
	taskInput                            domain.CreateUploadInput
	objectID, taskID                     uint64
	objectError, taskError, reserveError error
	afterReserve                         func()
	afterObject                          func(*domain.StoredObject)
	afterTask                            func(*domain.UploadTask)
	nilObject, nilTask                   bool
	task                                 *domain.UploadTask
}

func (transaction *createUploadTransactionStub) ReserveUserUploadSpace(ctx context.Context, sizeBytes uint64) error {
	transaction.events = append(transaction.events, "reserve")
	transaction.writeContexts = append(transaction.writeContexts, ctx)
	transaction.reserved = sizeBytes
	if transaction.afterReserve != nil {
		transaction.afterReserve()
	}
	return transaction.reserveError
}

func (transaction *createUploadTransactionStub) CreateUserStagedObject(ctx context.Context, input domain.CreateStagedObjectInput) (*domain.StoredObject, error) {
	transaction.events = append(transaction.events, "object")
	transaction.writeContexts = append(transaction.writeContexts, ctx)
	transaction.objectInput = input
	stamp := transaction.entries[11].CreatedAt
	staged := &domain.StoredObject{ID: transaction.objectID, OwnerUserID: transaction.profile.ID, Provider: input.Provider, Bucket: input.Bucket, ObjectKey: input.ObjectKey, FileSize: input.SizeBytes, Status: domain.StoredObjectStatusStaging, CreatedAt: stamp, UpdatedAt: stamp}
	if transaction.afterObject != nil {
		transaction.afterObject(staged)
	}
	if transaction.nilObject {
		return nil, transaction.objectError
	}
	return staged, transaction.objectError
}

func (transaction *createUploadTransactionStub) CreateUserUploadTask(ctx context.Context, objectID uint64, input domain.CreateUploadInput, expiresAt time.Time) (*domain.UploadTask, error) {
	transaction.events = append(transaction.events, "task")
	transaction.writeContexts = append(transaction.writeContexts, ctx)
	transaction.taskInput = input
	stamp := transaction.entries[11].CreatedAt
	transaction.task = &domain.UploadTask{ID: transaction.taskID, UserID: transaction.profile.ID, DeviceID: 19, StagedObjectID: objectID, TargetParentID: input.ParentID, RequestedName: input.Name, ExpectedSHA256: input.ExpectedSHA256, FileSize: input.SizeBytes, ReservedBytes: input.SizeBytes, UploadMode: "SINGLE", Status: domain.UploadStatusUploading, ExpiresAt: expiresAt, CreatedAt: stamp, UpdatedAt: stamp}
	if transaction.afterTask != nil {
		transaction.afterTask(transaction.task)
	}
	if transaction.nilTask {
		return nil, transaction.taskError
	}
	return transaction.task, transaction.taskError
}

func createUploadFixture() (domain.Actor, domain.CreateUploadInput, *createUploadTransactionStub) {
	actor, reader := fileEntryPathFixture()
	return actor, domain.CreateUploadInput{ParentID: 12, Name: " 学习.bin ", SizeBytes: 40}, &createUploadTransactionStub{fileEntryPathReaderStub: reader, objectID: 23, taskID: 29}
}

func TestCreateUploadServiceResults(t *testing.T) {
	for _, mode := range []string{"ordinary", "root parent", "zero bytes", "single limit", "zero digest", "binary digest", "maximum identifiers", "copy before transaction"} {
		t.Run(mode, func(t *testing.T) {
			actor, input, transaction := createUploadFixture()
			digest := sha256.Sum256([]byte("fixture"))
			switch mode {
			case "root parent":
				input.ParentID = 11
			case "zero bytes":
				input.SizeBytes = 0
			case "single limit":
				input.SizeBytes = domain.MaxSingleUploadSizeBytes
			case "zero digest":
				input.ExpectedSHA256 = new([sha256.Size]byte)
			case "binary digest", "copy before transaction":
				input.ExpectedSHA256 = &digest
			case "maximum identifiers":
				actor.UserID, actor.DeviceID, actor.SessionID = ^uint64(0), ^uint64(0), ^uint64(0)
				transaction.profile.ID = actor.UserID
				for _, entry := range transaction.entries {
					entry.UserID = actor.UserID
				}
				directory := transaction.entries[12]
				delete(transaction.entries, 12)
				directory.ID = ^uint64(0)
				transaction.entries[directory.ID] = directory
				input.ParentID = directory.ID
				transaction.objectID, transaction.taskID = ^uint64(0), ^uint64(0)
				transaction.afterTask = func(task *domain.UploadTask) { task.DeviceID = actor.DeviceID }
			}
			originalDigest := digest
			requestContext := context.Background()
			type transactionKey struct{}
			txContext := context.WithValue(requestContext, transactionKey{}, "upload transaction")
			calls, keyCalls := 0, 0
			creator := NewCreateUploadService("local_minio", "cloudnest", func(ctx context.Context, current domain.Actor, action func(context.Context, CreateUploadTransaction) error) error {
				calls++
				if ctx != requestContext || current != actor {
					t.Error("事务入参错误")
				}
				if mode == "copy before transaction" {
					digest[0] ^= 0xff
				}
				return action(txContext, transaction)
			})
			creator.generateObjectKey = func(userID uint64) (string, error) {
				keyCalls++
				if userID != actor.UserID || len(transaction.entryIDs) == 0 {
					t.Error("对象键生成范围或顺序错误")
				}
				return "users/fixture/objects/key", nil
			}
			earliest := time.Now().UTC().Truncate(time.Microsecond).Add(24 * time.Hour)
			task, err := creator.Create(requestContext, actor, input)
			latest := time.Now().UTC().Add(24 * time.Hour)
			if err != nil || task == nil || calls != 1 || keyCalls != 1 || !reflect.DeepEqual(transaction.events, []string{"reserve", "object", "task"}) || !reflect.DeepEqual(task, transaction.task) || task == transaction.task || transaction.reserved != input.SizeBytes || transaction.objectInput.SizeBytes != input.SizeBytes || transaction.taskInput.Name != "学习.bin" || task.ExpiresAt.Before(earliest) || task.ExpiresAt.After(latest) || task.ExpiresAt.Location() != time.UTC || task.ExpiresAt.Nanosecond()%1000 != 0 {
				t.Fatal("创建结果、调用顺序、参数或有效期错误", err)
			}
			for _, ctx := range append(transaction.contexts, transaction.writeContexts...) {
				if ctx != txContext {
					t.Fatal("读取和写入未使用同一事务上下文")
				}
			}
			for _, userID := range transaction.userIDs {
				if userID != actor.UserID {
					t.Fatal("路径读取用户范围错误")
				}
			}
			if input.Name != " 学习.bin " {
				t.Fatal("修改了原始展示名")
			}
			if mode == "binary digest" || mode == "copy before transaction" {
				if *task.ExpectedSHA256 != originalDigest {
					t.Fatal("声明摘要未提前复制")
				}
			}
			if task.ExpectedSHA256 != nil {
				if task.ExpectedSHA256 == transaction.task.ExpectedSHA256 || task.ExpectedSHA256 == input.ExpectedSHA256 {
					t.Fatal("任务摘要未独立持有")
				}
				task.ExpectedSHA256[0] ^= 0xff
				if *task.ExpectedSHA256 == *transaction.task.ExpectedSHA256 {
					t.Fatal("修改结果影响事务记录")
				}
			}
		})
	}
}

func TestCreateUploadServiceFailures(t *testing.T) {
	failure := errors.New("fixture create failure")
	for _, fixture := range []struct {
		name       string
		change     func(*createUploadTransactionStub)
		want       error
		wantEvents int
	}{
		{name: "missing parent", change: func(tx *createUploadTransactionStub) { delete(tx.entries, 12) }, want: domain.ErrFileEntryNotFound},
		{name: "hidden ancestor", change: func(tx *createUploadTransactionStub) { tx.entries[11].Status = "TRASHED" }, want: domain.ErrFileEntryNotFound},
		{name: "disabled account", change: func(tx *createUploadTransactionStub) { tx.profile.Status = "DISABLED" }, want: domain.ErrAccountDisabled},
		{name: "file target", change: func(tx *createUploadTransactionStub) { tx.entries[12].EntryType = "FILE"; tx.entries[12].FileSize = 1 }, want: domain.ErrFileEntryNotDirectory},
		{name: "path query failure", change: func(tx *createUploadTransactionStub) { tx.entryErrors[12] = failure }, want: failure},
		{name: "quota exceeded", change: func(tx *createUploadTransactionStub) { tx.reserveError = domain.ErrQuotaExceeded }, want: domain.ErrQuotaExceeded, wantEvents: 1},
		{name: "reserve failure", change: func(tx *createUploadTransactionStub) { tx.reserveError = failure }, want: failure, wantEvents: 1},
		{name: "object failure", change: func(tx *createUploadTransactionStub) { tx.objectError = failure }, want: failure, wantEvents: 2},
		{name: "object conflict", change: func(tx *createUploadTransactionStub) { tx.objectError = domain.ErrStoredObjectLocationConflict }, want: domain.ErrStoredObjectLocationConflict, wantEvents: 2},
		{name: "nil object", change: func(tx *createUploadTransactionStub) { tx.nilObject = true }, want: domain.ErrInvalidStoredObjectData, wantEvents: 2},
		{name: "wrong object scope", change: func(tx *createUploadTransactionStub) {
			tx.afterObject = func(obj *domain.StoredObject) { obj.OwnerUserID++ }
		}, want: domain.ErrInvalidStoredObjectData, wantEvents: 2},
		{name: "wrong object position", change: func(tx *createUploadTransactionStub) {
			tx.afterObject = func(obj *domain.StoredObject) { obj.ObjectKey = "other" }
		}, want: domain.ErrInvalidStoredObjectData, wantEvents: 2},
		{name: "wrong object state", change: func(tx *createUploadTransactionStub) {
			tx.afterObject = func(obj *domain.StoredObject) { obj.Status = domain.StoredObjectStatusReady }
		}, want: domain.ErrInvalidStoredObjectData, wantEvents: 2},
		{name: "wrong object size", change: func(tx *createUploadTransactionStub) {
			tx.afterObject = func(obj *domain.StoredObject) { obj.FileSize++ }
		}, want: domain.ErrInvalidStoredObjectData, wantEvents: 2},
		{name: "task failure", change: func(tx *createUploadTransactionStub) { tx.taskError = failure }, want: failure, wantEvents: 3},
		{name: "nil task", change: func(tx *createUploadTransactionStub) { tx.nilTask = true }, want: domain.ErrInvalidUploadTaskData, wantEvents: 3},
		{name: "wrong task owner", change: func(tx *createUploadTransactionStub) { tx.afterTask = func(task *domain.UploadTask) { task.UserID++ } }, want: domain.ErrInvalidUploadTaskData, wantEvents: 3},
		{name: "wrong task device", change: func(tx *createUploadTransactionStub) {
			tx.afterTask = func(task *domain.UploadTask) { task.DeviceID++ }
		}, want: domain.ErrInvalidUploadTaskData, wantEvents: 3},
		{name: "wrong task object", change: func(tx *createUploadTransactionStub) {
			tx.afterTask = func(task *domain.UploadTask) { task.StagedObjectID++ }
		}, want: domain.ErrInvalidUploadTaskData, wantEvents: 3},
		{name: "wrong task name", change: func(tx *createUploadTransactionStub) {
			tx.afterTask = func(task *domain.UploadTask) { task.RequestedName = "other" }
		}, want: domain.ErrInvalidUploadTaskData, wantEvents: 3},
		{name: "wrong task reservation", change: func(tx *createUploadTransactionStub) {
			tx.afterTask = func(task *domain.UploadTask) { task.ReservedBytes++ }
		}, want: domain.ErrInvalidUploadTaskData, wantEvents: 3},
		{name: "wrong task expiry", change: func(tx *createUploadTransactionStub) {
			tx.afterTask = func(task *domain.UploadTask) { task.ExpiresAt = task.ExpiresAt.Add(time.Second) }
		}, want: domain.ErrInvalidUploadTaskData, wantEvents: 3},
		{name: "unexpected lease", change: func(tx *createUploadTransactionStub) {
			tx.afterTask = func(task *domain.UploadTask) { task.LeaseOwner = new(string) }
		}, want: domain.ErrInvalidUploadTaskData, wantEvents: 3},
		{name: "unexpected completion", change: func(tx *createUploadTransactionStub) {
			tx.afterTask = func(task *domain.UploadTask) { task.CompletedAt = new(time.Time) }
		}, want: domain.ErrInvalidUploadTaskData, wantEvents: 3},
		{name: "unexpected hash", change: func(tx *createUploadTransactionStub) {
			tx.afterTask = func(task *domain.UploadTask) { task.ExpectedSHA256 = new([sha256.Size]byte) }
		}, want: domain.ErrInvalidUploadTaskData, wantEvents: 3},
		{name: "mutated task input hash", change: func(tx *createUploadTransactionStub) {
			tx.afterTask = func(task *domain.UploadTask) { task.ExpectedSHA256[0] ^= 0xff }
		}, want: domain.ErrInvalidUploadTaskData, wantEvents: 3},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			actor, input, tx := createUploadFixture()
			if fixture.name == "mutated task input hash" {
				input.ExpectedSHA256 = new([sha256.Size]byte)
			}
			fixture.change(tx)
			creator := NewCreateUploadService("local_minio", "cloudnest", func(ctx context.Context, _ domain.Actor, action func(context.Context, CreateUploadTransaction) error) error {
				return action(ctx, tx)
			})
			task, err := creator.Create(context.Background(), actor, input)
			if task != nil || !errors.Is(err, fixture.want) || len(tx.events) != fixture.wantEvents {
				t.Fatal("错误结果或失败短路错误", err, tx.events)
			}
			if input.ExpectedSHA256 != nil && *input.ExpectedSHA256 != [sha256.Size]byte{} {
				t.Fatal("任务依赖修改了来源摘要")
			}
		})
	}
}

func TestCreateUploadServiceBoundary(t *testing.T) {
	for _, mode := range []string{"nil context", "zero user", "zero device", "zero session", "zero parent", "invalid name", "oversize", "cancelled", "nil tx context", "nil tx store", "callback not invoked", "commit failure", "key failure", "invalid key"} {
		t.Run(mode, func(t *testing.T) {
			actor, input, tx := createUploadFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("fixture boundary failure")
			calls := 0
			creator := NewCreateUploadService("local_minio", "cloudnest", func(ctx context.Context, _ domain.Actor, action func(context.Context, CreateUploadTransaction) error) error {
				calls++
				switch mode {
				case "nil tx context":
					return action(nil, tx)
				case "nil tx store":
					return action(ctx, nil)
				case "callback not invoked":
					return nil
				}
				if err := action(ctx, tx); err != nil {
					return err
				}
				if mode == "commit failure" {
					return failure
				}
				return nil
			})
			switch mode {
			case "zero user":
				actor.UserID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero session":
				actor.SessionID = 0
			case "zero parent":
				input.ParentID = 0
			case "invalid name":
				input.Name = "private/name"
			case "oversize":
				input.SizeBytes = domain.MaxSingleUploadSizeBytes + 1
			case "cancelled":
				cancel()
			case "key failure":
				creator.generateObjectKey = func(uint64) (string, error) { return "", failure }
			case "invalid key":
				creator.generateObjectKey = func(uint64) (string, error) { return "", nil }
			}
			var requestContext context.Context = ctx
			if mode == "nil context" {
				requestContext = nil
			}
			task, err := creator.Create(requestContext, actor, input)
			if task != nil || err == nil {
				t.Fatal("无效边界返回了任务")
			}
			switch mode {
			case "nil context", "zero user", "zero device", "zero session", "zero parent", "invalid name", "oversize", "cancelled":
				if calls != 0 || len(tx.events) != 0 {
					t.Fatal("无效输入进入了事务")
				}
			case "key failure", "commit failure":
				if !errors.Is(err, failure) {
					t.Fatal("失败原因未保留", err)
				}
			}
			if mode == "key failure" || mode == "invalid key" {
				if len(tx.events) != 0 {
					t.Fatal("键失败后仍写入")
				}
			}
		})
	}
}

func TestCreateUploadServiceCancellation(t *testing.T) {
	for _, phase := range []string{"profile", "path", "key", "reserve", "object", "task", "after callback"} {
		t.Run(phase, func(t *testing.T) {
			actor, input, tx := createUploadFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("fixture cancelled dependency")
			creator := NewCreateUploadService("local_minio", "cloudnest", func(ctx context.Context, _ domain.Actor, action func(context.Context, CreateUploadTransaction) error) error {
				err := action(ctx, tx)
				if phase == "after callback" {
					cancel()
				}
				return err
			})
			switch phase {
			case "profile":
				tx.afterProfile = cancel
			case "path":
				tx.afterEntry = func(uint64) { cancel() }
			case "key":
				creator.generateObjectKey = func(uint64) (string, error) { cancel(); return "", failure }
			case "reserve":
				tx.afterReserve = cancel
				tx.reserveError = failure
			case "object":
				tx.afterObject = func(*domain.StoredObject) { cancel() }
				tx.objectError = failure
			case "task":
				tx.afterTask = func(*domain.UploadTask) { cancel() }
				tx.taskError = failure
			}
			task, err := creator.Create(ctx, actor, input)
			if task != nil || !errors.Is(err, context.Canceled) {
				t.Fatal("取消未返回空结果及上下文错误", err)
			}
		})
	}
}

func TestCreateUploadServiceDepth(t *testing.T) {
	for _, depth := range []int{31, 32} {
		t.Run(strconv.Itoa(depth), func(t *testing.T) {
			actor, input, tx := createUploadFixture()
			parentID := uint64(11)
			for level := 1; level <= depth; level++ {
				parent := parentID
				entry := *tx.entries[11]
				entry.ID = uint64(100 + level)
				entry.ParentID = &parent
				entry.Name = "nested"
				tx.entries[entry.ID] = &entry
				parentID = entry.ID
			}
			input.ParentID = parentID
			creator := NewCreateUploadService("local_minio", "cloudnest", func(ctx context.Context, _ domain.Actor, action func(context.Context, CreateUploadTransaction) error) error {
				return action(ctx, tx)
			})
			task, err := creator.Create(context.Background(), actor, input)
			if depth == 31 {
				if err != nil || task == nil {
					t.Fatal("最终深度32应可创建", err)
				}
			} else if task != nil || !errors.Is(err, domain.ErrFileEntryDepthExceeded) || len(tx.events) != 0 {
				t.Fatal("最终深度33应在写入前拒绝", err)
			}
		})
	}
}

func TestNewCreateUploadServiceDependencies(t *testing.T) {
	valid := func(context.Context, domain.Actor, func(context.Context, CreateUploadTransaction) error) error {
		return nil
	}
	for _, fixture := range []struct {
		name, provider, bucket string
		nilTransaction         bool
	}{
		{name: "missing transaction", provider: "local_minio", bucket: "cloudnest", nilTransaction: true},
		{name: "missing provider", bucket: "cloudnest"},
		{name: "missing bucket", provider: "local_minio"},
		{name: "long provider", provider: strings.Repeat("a", 33), bucket: "cloudnest"},
		{name: "long bucket", provider: "local_minio", bucket: strings.Repeat("a", 64)},
		{name: "non ASCII provider", provider: "存储", bucket: "cloudnest"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("装配错误应触发panic")
				}
			}()
			withTransaction := valid
			if fixture.nilTransaction {
				withTransaction = nil
			}
			NewCreateUploadService(fixture.provider, fixture.bucket, withTransaction)
		})
	}
}
