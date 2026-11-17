package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// uploadTaskIdentityRows 提供任务定位所需的两列。
type uploadTaskIdentityRows struct{ *idempotencyRecordRows }

func (*uploadTaskIdentityRows) Columns() []string { return []string{"id", "user_id"} }

// uploadTaskCreateFixture 提供 SINGLE/UPLOADING 的完整初始回读事实。
func uploadTaskCreateFixture(input domain.CreateUploadInput, objectID uint64, expiresAt time.Time) (domain.UploadTask, []driver.Value) {
	stamp := time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC)
	want := domain.UploadTask{
		ID: 19, UserID: 7, DeviceID: 11, StagedObjectID: objectID, TargetParentID: input.ParentID,
		RequestedName: input.Name, UploadMode: "SINGLE", ExpectedSHA256: copyUploadTaskValue(input.ExpectedSHA256),
		FileSize: input.SizeBytes, Status: domain.UploadStatusUploading, ReservedBytes: input.SizeBytes,
		ExpiresAt: expiresAt.UTC().Truncate(time.Microsecond), CreatedAt: stamp, UpdatedAt: stamp,
	}
	var digest driver.Value
	if input.ExpectedSHA256 != nil {
		digest = bytes.Clone(input.ExpectedSHA256[:])
	}
	values := []driver.Value{
		"19", "7", "11", strconv.FormatUint(objectID, 10), nil, strconv.FormatUint(input.ParentID, 10),
		input.Name, nil, "SINGLE", nil, digest, strconv.FormatUint(input.SizeBytes, 10), nil, nil, "UPLOADING", nil,
		strconv.FormatUint(input.SizeBytes, 10), nil, nil, "0", nil, int64(0), want.ExpiresAt, nil, nil, stamp, stamp, nil,
	}
	return want, values
}

func TestCreateUserUploadTaskResults(t *testing.T) {
	fault := errors.New("private upload creation failure")
	duplicate := &mysql.MySQLError{Number: 1062, Message: "private staged object duplicate"}
	fixtures := []struct {
		name                                                                    string
		rows                                                                    int64
		parentMissing, objectMissing, locateMissing, missing                    bool
		parentError, objectError, writeError, locateError, readError, wantError error
		parentChanges, objectChanges, taskChanges                               map[int]driver.Value
		locatorID, locatorOwner                                                 driver.Value
	}{
		{name: "ordinary task", rows: 1},
		{name: "zero-byte task", rows: 1},
		{name: "single size limit", rows: 1},
		{name: "maximum identifiers", rows: 1},
		{name: "declared digest and source copy", rows: 1},
		{name: "zero digest", rows: 1},
		{name: "UTC micro expiry", rows: 1},
		{name: "maximum expiry", rows: 1},
		{name: "missing parent", parentMissing: true, wantError: domain.ErrFileEntryNotFound},
		{name: "inactive parent", parentChanges: map[int]driver.Value{7: "TRASHED"}, wantError: domain.ErrFileEntryNotFound},
		{name: "file parent", parentChanges: map[int]driver.Value{4: "FILE"}, wantError: domain.ErrFileEntryNotDirectory},
		{name: "nonzero directory size", parentChanges: map[int]driver.Value{5: "1"}, wantError: domain.ErrInvalidFileEntryData},
		{name: "parent query failure", parentError: fault, wantError: fault},
		{name: "missing staged object", objectMissing: true, wantError: domain.ErrStoredObjectNotFound},
		{name: "staged query failure", objectError: fault, wantError: fault},
		{name: "wrong staged size", objectChanges: map[int]driver.Value{6: "1"}, wantError: domain.ErrInvalidUploadTaskData},
		{name: "wrong staged state", objectChanges: map[int]driver.Value{8: "ORPHANED"}, wantError: domain.ErrInvalidUploadTaskData},
		{name: "duplicate staged object", writeError: duplicate, wantError: domain.ErrUploadTaskStagedObjectConflict},
		{name: "wrapped duplicate", writeError: fmt.Errorf("driver: %w", duplicate), wantError: domain.ErrUploadTaskStagedObjectConflict},
		{name: "insert failure", writeError: fault, wantError: fault},
		{name: "zero affected rows", wantError: domain.ErrInvalidUploadTaskData},
		{name: "multiple affected rows", rows: 2, wantError: domain.ErrInvalidUploadTaskData},
		{name: "locator missing", rows: 1, locateMissing: true, wantError: domain.ErrInvalidUploadTaskData},
		{name: "locator failure", rows: 1, locateError: fault, wantError: fault},
		{name: "locator zero id", rows: 1, locatorID: "0", wantError: domain.ErrInvalidUploadTaskData},
		{name: "locator wrong owner", rows: 1, locatorOwner: "8", wantError: domain.ErrInvalidUploadTaskData},
		{name: "missing read back", rows: 1, missing: true, wantError: domain.ErrInvalidUploadTaskData},
		{name: "read back failure", rows: 1, readError: fault, wantError: fault},
		{name: "wrong device", rows: 1, taskChanges: map[int]driver.Value{2: "12"}, wantError: domain.ErrInvalidUploadTaskData},
		{name: "wrong staged reference", rows: 1, taskChanges: map[int]driver.Value{3: "24"}, wantError: domain.ErrInvalidUploadTaskData},
		{name: "wrong target", rows: 1, taskChanges: map[int]driver.Value{5: "14"}, wantError: domain.ErrInvalidUploadTaskData},
		{name: "wrong name", rows: 1, taskChanges: map[int]driver.Value{6: "other.bin"}, wantError: domain.ErrInvalidUploadTaskData},
		{name: "wrong reservation", rows: 1, taskChanges: map[int]driver.Value{16: "0"}, wantError: domain.ErrInvalidUploadTaskData},
		{name: "wrong initial status", rows: 1, taskChanges: map[int]driver.Value{14: "CREATING"}, wantError: domain.ErrInvalidUploadTaskData},
		{name: "unexpected digest", rows: 1, taskChanges: map[int]driver.Value{10: make([]byte, 32)}, wantError: domain.ErrInvalidUploadTaskData},
		{name: "unexpected lease version", rows: 1, taskChanges: map[int]driver.Value{19: "1"}, wantError: domain.ErrInvalidUploadTaskData},
		{name: "unexpected retry count", rows: 1, taskChanges: map[int]driver.Value{21: int64(1)}, wantError: domain.ErrInvalidUploadTaskData},
		{name: "outer callback failure", rows: 1},
		{name: "commit failure", rows: 1},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 准备可信身份、上传声明、目标目录和暂存对象。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			input := domain.CreateUploadInput{ParentID: 13, Name: "学习笔记.MD", SizeBytes: 2048}
			objectID := uint64(23)
			expiresAt := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
			switch fixture.name {
			case "zero-byte task":
				input.SizeBytes = 0
			case "single size limit":
				input.SizeBytes = domain.MaxSingleUploadSizeBytes
			case "maximum identifiers":
				input.ParentID, objectID, actor.DeviceID, actor.SessionID = ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)
			case "declared digest and source copy":
				digest := sha256.Sum256([]byte("fixture"))
				input.ExpectedSHA256 = &digest
			case "zero digest":
				input.ExpectedSHA256 = new([sha256.Size]byte)
			case "UTC micro expiry":
				expiresAt = expiresAt.In(time.FixedZone("fixture", 8*3600)).Add(789 * time.Nanosecond)
			case "maximum expiry":
				expiresAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
			}
			want, values := uploadTaskCreateFixture(input, objectID, expiresAt)
			want.DeviceID, values[2] = actor.DeviceID, strconv.FormatUint(actor.DeviceID, 10)
			if fixture.name == "maximum identifiers" {
				want.ID, values[0] = ^uint64(0), "18446744073709551615"
			}
			parentValues := []driver.Value{strconv.FormatUint(input.ParentID, 10), "7", nil, "", "DIR", "0", "1", "ACTIVE", want.CreatedAt, want.CreatedAt, want.CreatedAt}
			_, objectValues := stagedObjectCreateFixture(domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "objects/fixture", SizeBytes: input.SizeBytes})
			objectValues[0] = strconv.FormatUint(objectID, 10)
			for column, value := range fixture.parentChanges {
				parentValues[column] = value
			}
			for column, value := range fixture.objectChanges {
				objectValues[column] = value
			}
			for column, value := range fixture.taskChanges {
				values[column] = value
			}
			script := &loginTransactionScript{}
			if fixture.name == "commit failure" {
				script.commitError = fault
			}
			store := revokeSessionTestStore(t, actor, script)
			var transactionDeadline time.Time
			script.entryQuery = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				if !strings.Contains(strings.ReplaceAll(statement, "`", ""), "WHERE user_id = ? AND id = ?") || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != input.ParentID {
					t.Error("上传目标目录范围错误")
				}
				if fixture.parentMissing {
					parentValues = nil
				}
				return &fileEntryRows{values: parentValues}, fixture.parentError
			}
			script.objectQuery = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				if !strings.Contains(strings.ReplaceAll(statement, "`", ""), "WHERE owner_user_id = ? AND id = ?") || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != objectID {
					t.Error("上传暂存对象范围错误")
				}
				if fixture.objectMissing {
					objectValues = nil
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: objectValues}}, fixture.objectError
			}
			writes, reads := 0, 0
			script.insert = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				writes++
				if strings.ReplaceAll(statement, "`", "") != "INSERT INTO upload_tasks (user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,expected_sha256,file_size,status,reserved_bytes,expires_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)" || len(arguments) != 11 {
					t.Fatal("上传任务插入列或参数数量错误")
				}
				var expectedDigest []byte
				if want.ExpectedSHA256 != nil {
					expectedDigest = want.ExpectedSHA256[:]
				}
				expected := []any{actor.UserID, actor.DeviceID, objectID, input.ParentID, input.Name, "SINGLE", expectedDigest, input.SizeBytes, "UPLOADING", input.SizeBytes, want.ExpiresAt}
				for index, argument := range arguments {
					if !reflect.DeepEqual(argument.Value, expected[index]) {
						t.Error("上传任务插入值错误", index)
					}
				}
				if deadline, exists := ctx.Deadline(); !exists || !deadline.Equal(transactionDeadline) {
					t.Error("上传任务插入未服从事务期限")
				}
				if fixture.name == "declared digest and source copy" {
					input.ExpectedSHA256[0] ^= 0xff
				}
				return deviceInsertResult{rows: fixture.rows, idError: errors.New("LastInsertId should not be used")}, fixture.writeError
			}
			script.uploadQuery = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				reads++
				if deadline, exists := ctx.Deadline(); !exists || !deadline.Equal(transactionDeadline) {
					t.Error("上传任务回读未服从事务期限")
				}
				actualSQL := strings.ReplaceAll(statement, "`", "")
				if reads == 1 {
					if actualSQL != "SELECT id,user_id FROM upload_tasks WHERE user_id = ? AND staged_object_id = ? LIMIT ?" || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != objectID || arguments[2].Value != int64(1) {
						t.Error("任务标识定位范围错误")
					}
					identity := []driver.Value{strconv.FormatUint(want.ID, 10), "7"}
					if fixture.locatorID != nil {
						identity[0] = fixture.locatorID
					}
					if fixture.locatorOwner != nil {
						identity[1] = fixture.locatorOwner
					}
					if fixture.locateMissing {
						identity = nil
					}
					return &uploadTaskIdentityRows{&idempotencyRecordRows{values: identity}}, fixture.locateError
				}
				projection := "SELECT " + strings.Join((&uploadTaskMappingRows{}).Columns(), ",") + " FROM upload_tasks WHERE user_id = ? AND id = ? LIMIT ?"
				if actualSQL != projection || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != want.ID {
					t.Error("任务完整回读范围错误")
				}
				if fixture.missing {
					values = nil
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: values}}, fixture.readError
			}
			// 2. 同一事务中执行创建，错误和后续故障交由外层处理。
			var prepared *domain.UploadTask
			err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
				transactionDeadline, _ = txContext.Deadline()
				var err error
				prepared, err = txStore.CreateUserUploadTask(txContext, objectID, input, expiresAt)
				if err != nil {
					return err
				}
				if fixture.name == "outer callback failure" {
					return fault
				}
				return nil
			})
			// 3. 核对最终结果、事务顺序及错误链。
			if fixture.wantError == nil && fixture.name != "outer callback failure" && script.commitError == nil {
				if err != nil || !reflect.DeepEqual(prepared, &want) || writes != 1 || reads != 2 || script.sequence() != "begin,lock,device,session,entry,object,insert,upload,upload,commit" {
					t.Fatal("上传创建结果或顺序错误", err, script.sequence())
				}
				return
			}
			if err == nil || (fixture.wantError != nil && (!errors.Is(err, fixture.wantError) || prepared != nil)) ||
				(fixture.name == "outer callback failure" && !errors.Is(err, fault)) || (script.commitError != nil && !errors.Is(err, fault)) {
				t.Fatal("上传创建故障结果或原因错误", err)
			}
			lastEvent := "rollback"
			if script.commitError != nil {
				lastEvent = "commit"
			}
			if !strings.HasSuffix(script.sequence(), lastEvent) {
				t.Fatal("上传创建未正确结束事务", script.sequence())
			}
			if errors.Is(err, domain.ErrUploadTaskStagedObjectConflict) && strings.Contains(err.Error(), "private") {
				t.Fatal("重复引用错误泄露数据库原文")
			}
		})
	}
}

func TestCreateUserUploadTaskInputAndTransaction(t *testing.T) {
	for _, mode := range []string{
		"ordinary store", "bound store without transaction", "login transaction", "nil context", "wrong owner", "zero device", "zero session",
		"empty input", "invalid UTF-8 name", "oversized file", "zero object", "zero expiry", "expired expiry", "year overflow", "cancelled", "expired context",
	} {
		t.Run(mode, func(t *testing.T) {
			// 1. 无效声明或事务身份在业务 SQL 前结束。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			action := func(ctx context.Context, txStore *Store) error {
				input := domain.CreateUploadInput{ParentID: 13, Name: "file.bin", SizeBytes: 1}
				objectID := uint64(23)
				expiresAt := time.Now().Add(time.Hour)
				var wantError error
				switch mode {
				case "nil context":
					ctx = nil
				case "wrong owner":
					txStore.loginUserID, wantError = 8, domain.ErrInvalidSessionData
				case "zero device":
					txStore.authenticatedActor.DeviceID, wantError = 0, domain.ErrInvalidSessionData
				case "zero session":
					txStore.authenticatedActor.SessionID, wantError = 0, domain.ErrInvalidSessionData
				case "empty input":
					input, wantError = domain.CreateUploadInput{}, domain.ErrInvalidUploadTaskData
				case "invalid UTF-8 name":
					input.Name, wantError = "file\xff", domain.ErrInvalidUploadTaskData
				case "oversized file":
					input.SizeBytes, wantError = domain.MaxSingleUploadSizeBytes+1, domain.ErrInvalidUploadTaskData
				case "zero object":
					objectID, wantError = 0, domain.ErrInvalidUploadTaskData
				case "zero expiry":
					expiresAt, wantError = time.Time{}, domain.ErrInvalidUploadTaskData
				case "expired expiry":
					expiresAt, wantError = time.Now().Add(-time.Second), domain.ErrInvalidUploadTaskData
				case "year overflow":
					expiresAt, wantError = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), domain.ErrInvalidUploadTaskData
				case "cancelled":
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					ctx, wantError = cancelled, context.Canceled
				case "expired context":
					expired, release := context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer release()
					ctx, wantError = expired, context.DeadlineExceeded
				}
				created, err := txStore.CreateUserUploadTask(ctx, objectID, input, expiresAt)
				if created != nil || err == nil || (wantError != nil && !errors.Is(err, wantError)) {
					t.Fatal("无效上传创建调用未被正确拒绝", err)
				}
				return err
			}
			// 2. 分别经普通、登录和已认证事务入口检查零业务 SQL。
			var err error
			switch mode {
			case "ordinary store":
				err = action(context.Background(), store)
			case "bound store without transaction":
				err = action(context.Background(), &Store{database: store.database, loginUserID: 7, authenticatedActor: &actor})
			case "login transaction":
				err = store.WithLoginUserTransaction(context.Background(), 7, action)
			default:
				err = store.WithUserTransaction(context.Background(), actor, action)
			}
			if err == nil || strings.Contains(script.sequence(), "entry") || strings.Contains(script.sequence(), "object") || strings.Contains(script.sequence(), "insert") || strings.Contains(script.sequence(), "upload") || strings.Contains(script.sequence(), "commit") {
				t.Fatal("无效上传调用执行了业务查询、写入或提交", err, script.sequence())
			}
		})
	}
}

func TestCreateUserUploadTaskCancellationAndExpiry(t *testing.T) {
	for _, phase := range []string{
		"during target", "after target", "during staged", "after staged", "during insert", "after insert", "after duplicate",
		"during locator", "after locator", "during full read", "after full read", "expiry before insert", "expiry after full read",
	} {
		t.Run(phase, func(t *testing.T) {
			// 1. 在每个业务阶段注入取消，并单独检查途中到期。
			timeout := 50 * time.Millisecond
			if strings.HasPrefix(phase, "expiry") {
				timeout = time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			input := domain.CreateUploadInput{ParentID: 13, Name: "file.bin", SizeBytes: 1}
			expiresAt := time.Now().Add(time.Hour)
			if strings.HasPrefix(phase, "expiry") {
				expiresAt = time.Now().Add(100 * time.Millisecond)
			}
			want, values := uploadTaskCreateFixture(input, 23, expiresAt)
			_, objectValues := stagedObjectCreateFixture(domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "objects/expiry", SizeBytes: 1})
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			script.entryQuery = func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if phase == "during target" {
					<-queryContext.Done()
					return nil, queryContext.Err()
				}
				if phase == "after target" {
					cancel()
				}
				return &fileEntryRows{values: []driver.Value{"13", "7", nil, "", "DIR", "0", "1", "ACTIVE", want.CreatedAt, want.CreatedAt, want.CreatedAt}}, nil
			}
			script.objectQuery = func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if phase == "during staged" {
					<-queryContext.Done()
					return nil, queryContext.Err()
				}
				if phase == "after staged" {
					cancel()
				}
				if phase == "expiry before insert" {
					time.Sleep(time.Until(expiresAt) + 5*time.Millisecond)
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: objectValues}}, nil
			}
			script.insert = func(writeContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if phase == "during insert" {
					<-writeContext.Done()
					return nil, writeContext.Err()
				}
				if phase == "after insert" || phase == "after duplicate" {
					cancel()
				}
				if phase == "after duplicate" {
					return nil, &mysql.MySQLError{Number: 1062, Message: "private cancelled staged reference"}
				}
				return driver.RowsAffected(1), nil
			}
			reads := 0
			script.uploadQuery = func(readContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				reads++
				if (reads == 1 && phase == "during locator") || (reads == 2 && phase == "during full read") {
					<-readContext.Done()
					return nil, readContext.Err()
				}
				if (reads == 1 && phase == "after locator") || (reads == 2 && phase == "after full read") {
					cancel()
				}
				if reads == 1 {
					return &uploadTaskIdentityRows{&idempotencyRecordRows{values: []driver.Value{"19", "7"}}}, nil
				}
				if phase == "expiry after full read" {
					time.Sleep(time.Until(expiresAt) + 5*time.Millisecond)
				}
				return &uploadTaskReadRows{&idempotencyRecordRows{values: values}}, nil
			}
			// 2. 失败返回 nil 并交由外层回滚，等待真实回滚事件。
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				created, err := txStore.CreateUserUploadTask(txContext, 23, input, expiresAt)
				if created != nil {
					t.Error("取消或到期后返回了上传任务")
				}
				return err
			})
			wantError := error(context.Canceled)
			if strings.HasPrefix(phase, "during") {
				wantError = context.DeadlineExceeded
			}
			if strings.HasPrefix(phase, "expiry") {
				wantError = domain.ErrInvalidUploadTaskData
			}
			if !errors.Is(err, wantError) || strings.Contains(script.sequence(), "commit") {
				t.Fatal("上传取消/到期原因或事务结束错误", err, script.sequence())
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("上传取消或到期后未回滚")
			}
		})
	}
}
