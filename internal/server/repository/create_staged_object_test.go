package repository

import (
	"context"
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

// stagedObjectIdentityRows 提供位置查询所需的标识与归属两列。
type stagedObjectIdentityRows struct{ *idempotencyRecordRows }

func (*stagedObjectIdentityRows) Columns() []string { return []string{"id", "owner_user_id"} }

// stagedObjectCreateFixture 为创建回读准备完整的初始对象事实。
func stagedObjectCreateFixture(input domain.CreateStagedObjectInput) (domain.StoredObject, []driver.Value) {
	stamp := time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC)
	want := domain.StoredObject{
		ID: 23, OwnerUserID: 7, Provider: input.Provider, Bucket: input.Bucket, ObjectKey: input.ObjectKey,
		FileSize: input.SizeBytes, Status: domain.StoredObjectStatusStaging, CreatedAt: stamp, UpdatedAt: stamp,
	}
	values := []driver.Value{
		"23", "7", input.Provider, input.Bucket, input.ObjectKey, nil, strconv.FormatUint(input.SizeBytes, 10), nil,
		"STAGING", nil, nil, nil, nil, nil, nil, stamp, stamp,
	}
	return want, values
}

func TestCreateUserStagedObjectResults(t *testing.T) {
	writeFailure := errors.New("private staged insert failure")
	locateFailure := errors.New("private staged locate failure")
	readFailure := errors.New("private staged read failure")
	duplicate := &mysql.MySQLError{Number: 1062, Message: "private object location"}
	foreignKey := &mysql.MySQLError{Number: 1452, Message: "private owner foreign key"}
	fixtures := []struct {
		name                                          string
		rows                                          int64
		writeError, locateError, readError, wantError error
		locateMissing, readMissing                    bool
		locatorID, locatorOwner                       driver.Value
		changes                                       map[int]driver.Value
		outerError                                    error
	}{
		{name: "ordinary object", rows: 1},
		{name: "zero-byte object", rows: 1},
		{name: "maximum unsigned object and size", rows: 1},
		{name: "maximum text lengths", rows: 1},
		{name: "duplicate location", writeError: duplicate, wantError: domain.ErrStoredObjectLocationConflict},
		{name: "wrapped duplicate", writeError: fmt.Errorf("driver: %w", duplicate), wantError: domain.ErrStoredObjectLocationConflict},
		{name: "foreign key failure", writeError: foreignKey, wantError: foreignKey},
		{name: "write failure", writeError: writeFailure, wantError: writeFailure},
		{name: "zero affected rows", wantError: domain.ErrInvalidStoredObjectData},
		{name: "multiple affected rows", rows: 2, wantError: domain.ErrInvalidStoredObjectData},
		{name: "missing locator", rows: 1, locateMissing: true, wantError: domain.ErrInvalidStoredObjectData},
		{name: "locator query failure", rows: 1, locateError: locateFailure, wantError: locateFailure},
		{name: "zero locator id", rows: 1, locatorID: "0", wantError: domain.ErrInvalidStoredObjectData},
		{name: "wrong locator owner", rows: 1, locatorOwner: "8", wantError: domain.ErrInvalidStoredObjectData},
		{name: "missing full record", rows: 1, readMissing: true, wantError: domain.ErrInvalidStoredObjectData},
		{name: "full query failure", rows: 1, readError: readFailure, wantError: readFailure},
		{name: "wrong provider", rows: 1, changes: map[int]driver.Value{2: "other"}, wantError: domain.ErrInvalidStoredObjectData},
		{name: "wrong bucket", rows: 1, changes: map[int]driver.Value{3: "other-bucket"}, wantError: domain.ErrInvalidStoredObjectData},
		{name: "wrong key", rows: 1, changes: map[int]driver.Value{4: "other-key"}, wantError: domain.ErrInvalidStoredObjectData},
		{name: "wrong size", rows: 1, changes: map[int]driver.Value{6: "1"}, wantError: domain.ErrInvalidStoredObjectData},
		{name: "wrong status", rows: 1, changes: map[int]driver.Value{8: "DELETED"}, wantError: domain.ErrInvalidStoredObjectData},
		{name: "outer rollback", rows: 1, outerError: errors.New("outer staged failure")},
		{name: "commit failure", rows: 1},
	}
	for _, column := range []int{5, 7, 9, 10, 11, 12, 13, 14} {
		var value driver.Value = "unexpected"
		if column == 7 || column == 9 {
			value = make([]byte, 32)
		}
		if column == 11 || column == 12 || column == 13 {
			value = time.Date(2026, 11, 17, 8, 0, 0, 123456000, time.UTC)
		}
		fixtures = append(fixtures, struct {
			name                                          string
			rows                                          int64
			writeError, locateError, readError, wantError error
			locateMissing, readMissing                    bool
			locatorID, locatorOwner                       driver.Value
			changes                                       map[int]driver.Value
			outerError                                    error
		}{name: "unexpected " + (&storedObjectMappingRows{}).Columns()[column], rows: 1, changes: map[int]driver.Value{column: value}, wantError: domain.ErrInvalidStoredObjectData})
	}
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 沿用已认证事务脚本，准备声明和初始对象回读。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			input := domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "users/7/objects/test", SizeBytes: 2048}
			if fixture.name == "zero-byte object" {
				input.SizeBytes = 0
			}
			if fixture.name == "maximum unsigned object and size" {
				input.SizeBytes, actor.DeviceID, actor.SessionID = ^uint64(0), ^uint64(0), ^uint64(0)
			}
			if fixture.name == "maximum text lengths" {
				input.Provider, input.Bucket, input.ObjectKey = strings.Repeat("p", 32), strings.Repeat("b", 63), strings.Repeat("k", 255)
			}
			want, values := stagedObjectCreateFixture(input)
			if fixture.name == "maximum unsigned object and size" {
				want.ID, values[0] = ^uint64(0), "18446744073709551615"
			}
			for column, value := range fixture.changes {
				values[column] = value
			}
			script := &loginTransactionScript{}
			if fixture.name == "commit failure" {
				script.commitError = errors.New("staged commit failure")
			}
			store := revokeSessionTestStore(t, actor, script)
			reads, writes := 0, 0
			var transactionDeadline time.Time
			script.insert = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				writes++
				if strings.ReplaceAll(statement, "`", "") != "INSERT INTO stored_objects (owner_user_id,provider,bucket,object_key,file_size,status) VALUES (?,?,?,?,?,?)" || len(arguments) != 6 {
					t.Fatal("暂存插入字段或占位符错误")
				}
				expected := []any{actor.UserID, input.Provider, input.Bucket, input.ObjectKey, input.SizeBytes, "STAGING"}
				for index, argument := range arguments {
					if !reflect.DeepEqual(argument.Value, expected[index]) {
						t.Error("暂存插入绑定值错误", index)
					}
				}
				if deadline, exists := ctx.Deadline(); !exists || !deadline.Equal(transactionDeadline) {
					t.Error("暂存插入未服从外层事务期限")
				}
				return deviceInsertResult{rows: fixture.rows, idError: errors.New("LastInsertId should not be used")}, fixture.writeError
			}
			script.objectQuery = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				reads++
				actualSQL := strings.ReplaceAll(statement, "`", "")
				if deadline, exists := ctx.Deadline(); !exists || !deadline.Equal(transactionDeadline) {
					t.Error("暂存回读未服从外层事务期限")
				}
				if reads == 1 {
					if actualSQL != "SELECT id,owner_user_id FROM stored_objects WHERE owner_user_id = ? AND provider = ? AND bucket = ? AND object_key = ? LIMIT ?" || len(arguments) != 5 ||
						arguments[0].Value != actor.UserID || arguments[1].Value != input.Provider || arguments[2].Value != input.Bucket || arguments[3].Value != input.ObjectKey || arguments[4].Value != int64(1) {
						t.Error("暂存标识定位范围错误")
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
					return &stagedObjectIdentityRows{&idempotencyRecordRows{values: identity}}, fixture.locateError
				}
				projection := "SELECT " + strings.Join((&storedObjectMappingRows{}).Columns(), ",") + " FROM stored_objects WHERE owner_user_id = ? AND id = ? LIMIT ?"
				if actualSQL != projection || len(arguments) != 3 || arguments[0].Value != actor.UserID || arguments[1].Value != want.ID || arguments[2].Value != int64(1) {
					t.Error("暂存完整回读范围错误")
				}
				if fixture.readMissing {
					values = nil
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: values}}, fixture.readError
			}
			// 2. 本方法错误交回事务入口，外层失败可回滚先前插入。
			var prepared *domain.StoredObject
			err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
				transactionDeadline, _ = txContext.Deadline()
				var err error
				prepared, err = txStore.CreateUserStagedObject(txContext, input)
				if err != nil {
					if errors.Is(err, domain.ErrStoredObjectLocationConflict) && err.Error() != "存储对象位置已存在" {
						t.Error("创建方法的位置冲突提示包含原始数据库数据")
					}
					return err
				}
				return fixture.outerError
			})
			// 3. 核对结果、错误链及提交/回滚的完整顺序。
			if fixture.wantError == nil && fixture.outerError == nil && script.commitError == nil {
				if err != nil || !reflect.DeepEqual(prepared, &want) || writes != 1 || reads != 2 || script.sequence() != "begin,lock,device,session,insert,object,object,commit" {
					t.Fatal("暂存创建结果或事务调用顺序错误", err, script.sequence())
				}
				return
			}
			if err == nil || (fixture.wantError != nil && (!errors.Is(err, fixture.wantError) || prepared != nil)) ||
				(fixture.outerError != nil && !errors.Is(err, fixture.outerError)) || (script.commitError != nil && !errors.Is(err, script.commitError)) {
				t.Fatal("暂存创建故障未返回 nil 或丢失错误链", err)
			}
			lastEvent := "rollback"
			if script.commitError != nil {
				lastEvent = "commit"
			}
			if !strings.HasSuffix(script.sequence(), lastEvent) || writes != 1 {
				t.Fatal("创建故障的写入次数或事务结束错误", script.sequence())
			}
			if errors.Is(err, domain.ErrStoredObjectLocationConflict) && (strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), input.ObjectKey)) {
				t.Fatal("位置冲突提示包含原始数据库数据")
			}
		})
	}
}

func TestCreateUserStagedObjectInputAndTransaction(t *testing.T) {
	for _, mode := range []string{
		"ordinary store", "bound store without transaction", "login transaction", "nil context", "wrong owner",
		"zero device", "zero session", "empty input", "non-ASCII location", "long location", "cancelled", "expired",
	} {
		t.Run(mode, func(t *testing.T) {
			// 1. 覆盖事务身份、输入及预先取消，验证写入前拒绝。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			action := func(ctx context.Context, txStore *Store) error {
				input := domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "objects/test"}
				var wantError error
				switch mode {
				case "nil context":
					ctx = nil
				case "wrong owner":
					txStore.loginUserID = 8
					wantError = domain.ErrInvalidSessionData
				case "zero device":
					txStore.authenticatedActor.DeviceID = 0
					wantError = domain.ErrInvalidSessionData
				case "zero session":
					txStore.authenticatedActor.SessionID = 0
					wantError = domain.ErrInvalidSessionData
				case "empty input":
					input = domain.CreateStagedObjectInput{}
					wantError = domain.ErrInvalidStoredObjectData
				case "non-ASCII location":
					input.ObjectKey = "对象"
					wantError = domain.ErrInvalidStoredObjectData
				case "long location":
					input.ObjectKey = strings.Repeat("k", 256)
					wantError = domain.ErrInvalidStoredObjectData
				case "cancelled":
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					ctx, wantError = cancelled, context.Canceled
				case "expired":
					expired, release := context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer release()
					ctx, wantError = expired, context.DeadlineExceeded
				}
				created, err := txStore.CreateUserStagedObject(ctx, input)
				if created != nil || err == nil || (wantError != nil && !errors.Is(err, wantError)) {
					t.Fatal("无效暂存创建调用未被正确拒绝", err)
				}
				return err
			}
			// 2. 经不同入口执行，确认仅认证检查发生且没有业务 SQL。
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
			if err == nil || strings.Contains(script.sequence(), "insert") || strings.Contains(script.sequence(), "object") || strings.Contains(script.sequence(), "commit") {
				t.Fatal("无效调用执行了暂存写入或提交", err, script.sequence())
			}
		})
	}
}

func TestCreateUserStagedObjectCancellation(t *testing.T) {
	for _, phase := range []string{"during insert", "after insert", "after duplicate", "during locator", "after locator", "during full read", "after full read"} {
		t.Run(phase, func(t *testing.T) {
			// 1. 在插入、位置定位和完整回读分别注入取消或短期限。
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			input := domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "objects/cancelled", SizeBytes: 1}
			_, values := stagedObjectCreateFixture(input)
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			script.insert = func(writeContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if phase == "during insert" {
					<-writeContext.Done()
					return nil, writeContext.Err()
				}
				if phase == "after insert" || phase == "after duplicate" {
					cancel()
				}
				if phase == "after duplicate" {
					return nil, &mysql.MySQLError{Number: 1062, Message: "private cancelled location"}
				}
				return driver.RowsAffected(1), nil
			}
			reads := 0
			script.objectQuery = func(readContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				reads++
				if (reads == 1 && phase == "during locator") || (reads == 2 && phase == "during full read") {
					<-readContext.Done()
					return nil, readContext.Err()
				}
				if (reads == 1 && phase == "after locator") || (reads == 2 && phase == "after full read") {
					cancel()
				}
				if reads == 1 {
					return &stagedObjectIdentityRows{&idempotencyRecordRows{values: []driver.Value{"23", "7"}}}, nil
				}
				return &storedObjectReadRows{&idempotencyRecordRows{values: values}}, nil
			}
			// 2. 错误交回事务入口，等待真实 database/sql 回滚事件。
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				created, err := txStore.CreateUserStagedObject(txContext, input)
				if created != nil {
					t.Error("取消后返回了暂存对象")
				}
				return err
			})
			wantError := error(context.Canceled)
			if strings.HasPrefix(phase, "during") {
				wantError = context.DeadlineExceeded
			}
			if !errors.Is(err, wantError) || strings.Contains(script.sequence(), "commit") {
				t.Fatal("暂存取消原因丢失或仍提交", err, script.sequence())
			}
			select {
			case <-script.rolledBack:
			case <-time.After(time.Second):
				t.Fatal("暂存取消后未回滚")
			}
		})
	}
}
