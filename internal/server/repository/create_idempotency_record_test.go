package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func idempotencyResultFixture() domain.IdempotencyResultInput {
	resourceID := uint64(23)
	return domain.IdempotencyResultInput{
		Operation: domain.CreateDirectoryOperation, IdempotencyKey: "Request-007", RequestHash: sha256.Sum256([]byte("fixture request")),
		ResourceType: "ENTRY", ResourceID: &resourceID, ResponseStatus: 201,
		ResponseBody: json.RawMessage(`{"entry_id":"23","name":"学习资料"}`),
		ExpiresAt:    time.Now().UTC().Truncate(time.Microsecond).Add(8 * 24 * time.Hour),
	}
}

func TestCreateUserIdempotencyRecordResults(t *testing.T) {
	writeFailure, readFailure := errors.New("private write failure"), errors.New("private read failure")
	duplicate := &mysql.MySQLError{Number: 1062, Message: "private duplicate key"}
	foreignKey := &mysql.MySQLError{Number: 1452, Message: "private foreign key"}
	for _, testCase := range []struct {
		name, changedField                   string
		rows                                 int64
		writeErr, readErr, outerErr, wantErr error
		missing                              bool
	}{
		{name: "complete result", rows: 1},
		{name: "sql null body and resource", rows: 1},
		{name: "json null body", rows: 1},
		{name: "json array body", rows: 1},
		{name: "utc micro expiry", rows: 1},
		{name: "maximum text capacities", rows: 1},
		{name: "full unsigned result ids", rows: 1},
		{name: "duplicate", writeErr: duplicate, wantErr: domain.ErrIdempotencyRecordAlreadyExists},
		{name: "wrapped duplicate", writeErr: fmt.Errorf("driver: %w", duplicate), wantErr: domain.ErrIdempotencyRecordAlreadyExists},
		{name: "foreign key error", writeErr: foreignKey, wantErr: foreignKey},
		{name: "write error", writeErr: writeFailure, wantErr: writeFailure},
		{name: "zero affected rows", wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "multiple affected rows", rows: 2, wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "missing read back", rows: 1, missing: true, wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "read error", rows: 1, readErr: readFailure, wantErr: readFailure},
		{name: "wrong hash", rows: 1, changedField: "hash", wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "wrong resource type", rows: 1, changedField: "type", wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "wrong resource id", rows: 1, changedField: "resource", wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "wrong status", rows: 1, changedField: "status", wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "wrong expiry", rows: 1, changedField: "expiry", wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "wrong body nullness", rows: 1, changedField: "body", wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "outer callback failure", rows: 1, outerErr: errors.New("outer failure")},
		{name: "commit failure", rows: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 准备写入字段和读回结果，沿用真实 database/sql 事务脚本。
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			input := idempotencyResultFixture()
			recordID := uint64(19)
			switch testCase.name {
			case "sql null body and resource":
				input.ResourceID, input.ResponseBody = nil, nil
			case "json null body":
				input.ResponseBody = json.RawMessage("null")
			case "json array body":
				input.ResponseBody = json.RawMessage("[]")
			case "utc micro expiry":
				input.ExpiresAt = input.ExpiresAt.In(time.FixedZone("fixture", 8*3600)).Add(789 * time.Nanosecond)
			case "maximum text capacities":
				input.Operation, input.IdempotencyKey, input.ResourceType = strings.Repeat("O", 64), strings.Repeat("K", 128), strings.Repeat("R", 32)
			case "full unsigned result ids":
				maximum := ^uint64(0)
				recordID, input.ResourceID = maximum, &maximum
			}
			expiry := input.ExpiresAt.UTC().Truncate(time.Microsecond)
			created := time.Now().UTC().Truncate(time.Microsecond)
			var resource, body driver.Value
			if input.ResourceID != nil {
				resource = fmt.Sprint(*input.ResourceID)
			}
			if input.ResponseBody != nil {
				body = []byte(input.ResponseBody)
			}
			values := []driver.Value{fmt.Sprint(recordID), "7", input.Operation, input.IdempotencyKey, input.RequestHash[:], input.ResourceType, resource, int64(input.ResponseStatus), body, expiry, created}
			switch testCase.changedField {
			case "hash":
				values[4] = make([]byte, sha256.Size)
			case "type":
				values[5] = "OTHER"
			case "resource":
				values[6] = int64(99)
			case "status":
				values[7] = int64(202)
			case "expiry":
				values[9] = expiry.Add(time.Second)
			case "body":
				values[8] = nil
			}
			if testCase.missing {
				values = nil
			}
			script := &loginTransactionScript{}
			if testCase.name == "commit failure" {
				script.commitError = errors.New("commit failure")
			}
			store := revokeSessionTestStore(t, actor, script)
			writes, reads := 0, 0
			script.insert = func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Result, error) {
				writes++
				if strings.ReplaceAll(statement, "`", "") != "INSERT INTO idempotency_records (user_id,operation,idempotency_key,request_hash,resource_type,resource_id,response_status,response_body,expires_at) VALUES (?,?,?,?,?,?,?,?,?)" || len(arguments) != 9 {
					t.Fatal("幂等结果写入字段或占位符错误")
				}
				var expectedResource, expectedBody any
				if input.ResourceID != nil {
					expectedResource = *input.ResourceID
				}
				if input.ResponseBody != nil {
					expectedBody = string(input.ResponseBody)
				}
				expected := []any{actor.UserID, input.Operation, input.IdempotencyKey, input.RequestHash[:], input.ResourceType, expectedResource, int64(input.ResponseStatus), expectedBody, expiry}
				for index, argument := range arguments {
					if !reflect.DeepEqual(argument.Value, expected[index]) {
						t.Error("写入归属、摘要、JSON 文本、空值或期限参数错误", index)
					}
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("保存结果未服从事务期限")
				}
				return deviceInsertResult{rows: testCase.rows, idError: errors.New("LastInsertId should not be used")}, testCase.writeErr
			}
			script.idempotencyQuery = func(_ context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				reads++
				if !strings.Contains(statement, "user_id = ? AND operation = ? AND idempotency_key = ?") || len(arguments) != 4 || arguments[0].Value != actor.UserID || arguments[1].Value != input.Operation || arguments[2].Value != input.IdempotencyKey {
					t.Error("保存后回读范围错误")
				}
				return &idempotencyRecordRows{values: values}, testCase.readErr
			}

			// 2. 操作失败交回事务入口，回读结果在提交成功后才向外确认。
			var prepared *domain.IdempotencyRecord
			err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
				var err error
				prepared, err = txStore.CreateUserIdempotencyRecord(txContext, input)
				if err != nil {
					return err
				}
				return testCase.outerErr
			})
			failed := testCase.wantErr != nil || testCase.outerErr != nil || script.commitError != nil
			if !failed {
				if err != nil || prepared == nil || prepared.ID != recordID || prepared.UserID != actor.UserID || prepared.RequestHash != input.RequestHash || prepared.CreatedAt != created || !prepared.ExpiresAt.Equal(expiry) || writes != 1 || reads != 1 || script.sequence() != "begin,lock,device,session,insert,idempotency,commit" {
					t.Fatal("保存、回读或提交结果错误", err, script.sequence())
				}
				return
			}
			lastEvent := "rollback"
			if script.commitError != nil {
				lastEvent = "commit"
			}
			if err == nil || (testCase.wantErr != nil && !errors.Is(err, testCase.wantErr)) || (testCase.outerErr != nil && !errors.Is(err, testCase.outerErr)) || (script.commitError != nil && !errors.Is(err, script.commitError)) || !strings.HasSuffix(script.sequence(), lastEvent) {
				t.Fatal("保存失败未保留错误原因或正确事务结束事件", err, script.sequence())
			}
			if testCase.wantErr != nil && prepared != nil {
				t.Fatal("操作失败仍返回部分记录")
			}
			if errors.Is(err, domain.ErrIdempotencyRecordAlreadyExists) && strings.Contains(err.Error(), "private") {
				t.Fatal("重复键错误回显数据库内容")
			}
		})
	}
}

func TestCreateUserIdempotencyRecordInput(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	script := &loginTransactionScript{}
	store := revokeSessionTestStore(t, actor, script)
	for _, candidate := range []*Store{store, {database: store.database, loginUserID: actor.UserID, authenticatedActor: &actor}} {
		if record, err := candidate.CreateUserIdempotencyRecord(context.Background(), idempotencyResultFixture()); record != nil || err == nil || script.sequence() != "" {
			t.Fatal("普通连接池被允许保存结果")
		}
	}
	err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
		for _, change := range []func(*domain.IdempotencyResultInput){
			func(input *domain.IdempotencyResultInput) { input.Operation = "" },
			func(input *domain.IdempotencyResultInput) { input.Operation = strings.Repeat("a", 65) },
			func(input *domain.IdempotencyResultInput) { input.Operation = "v1.操作" },
			func(input *domain.IdempotencyResultInput) { input.IdempotencyKey = "" },
			func(input *domain.IdempotencyResultInput) { input.IdempotencyKey = "a,b" },
			func(input *domain.IdempotencyResultInput) { input.IdempotencyKey = strings.Repeat("a", 129) },
			func(input *domain.IdempotencyResultInput) { input.ResourceType = "" },
			func(input *domain.IdempotencyResultInput) { input.ResourceType = strings.Repeat("a", 33) },
			func(input *domain.IdempotencyResultInput) { input.ResourceType = "资源" },
			func(input *domain.IdempotencyResultInput) { *input.ResourceID = 0 },
			func(input *domain.IdempotencyResultInput) { input.ResponseStatus = 99 },
			func(input *domain.IdempotencyResultInput) { input.ResponseStatus = 600 },
			func(input *domain.IdempotencyResultInput) { input.ResponseBody = json.RawMessage{} },
			func(input *domain.IdempotencyResultInput) { input.ResponseBody = json.RawMessage(`{"private":`) },
			func(input *domain.IdempotencyResultInput) { input.ResponseBody = json.RawMessage("\"private\xff\"") },
			func(input *domain.IdempotencyResultInput) { input.ExpiresAt = time.Time{} },
			func(input *domain.IdempotencyResultInput) { input.ExpiresAt = time.Now().Add(-time.Hour) },
			func(input *domain.IdempotencyResultInput) {
				input.ExpiresAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			},
		} {
			input := idempotencyResultFixture()
			change(&input)
			if record, err := txStore.CreateUserIdempotencyRecord(txContext, input); record != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Error("无效元数据未返回空结果和固定错误", err)
			}
		}
		if record, err := txStore.CreateUserIdempotencyRecord(nil, idempotencyResultFixture()); record != nil || err == nil {
			t.Error("缺少上下文仍保存结果")
		}
		cancelled, cancel := context.WithCancel(txContext)
		cancel()
		if record, err := txStore.CreateUserIdempotencyRecord(cancelled, idempotencyResultFixture()); record != nil || !errors.Is(err, context.Canceled) {
			t.Error("预先取消仍保存结果", err)
		}
		return nil
	})
	if err != nil || script.sequence() != "begin,lock,device,session,commit" {
		t.Fatal("无效输入访问了插入或回读路径", err, script.sequence())
	}
}

func TestCreateUserIdempotencyRecordCancellation(t *testing.T) {
	for _, phase := range []string{"during insert", "after insert", "after duplicate", "after read"} {
		t.Run(phase, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			script := &loginTransactionScript{}
			store := revokeSessionTestStore(t, actor, script)
			script.insert = func(writeContext context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
				if phase == "during insert" {
					<-writeContext.Done()
					return nil, writeContext.Err()
				}
				if phase != "after read" {
					cancel()
				}
				if phase == "after duplicate" {
					return nil, &mysql.MySQLError{Number: 1062, Message: "private duplicate"}
				}
				return driver.RowsAffected(1), nil
			}
			script.idempotencyQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
				cancel()
				return &idempotencyRecordRows{}, nil
			}
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				record, err := txStore.CreateUserIdempotencyRecord(txContext, idempotencyResultFixture())
				if record != nil {
					t.Error("取消后仍返回结果记录")
				}
				return err
			})
			want := error(context.Canceled)
			if phase == "during insert" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) || strings.Contains(script.sequence(), "commit") {
				t.Fatal("取消原因丢失或仍提交", err)
			}
		})
	}
}

func TestCreateUserIdempotencyRecordCopies(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	input := idempotencyResultFixture()
	expectedHash, expectedBody, expectedResource := input.RequestHash, bytes.Clone(input.ResponseBody), *input.ResourceID
	script := &loginTransactionScript{}
	store := revokeSessionTestStore(t, actor, script)
	script.insert = func(context.Context, string, []driver.NamedValue) (driver.Result, error) {
		input.RequestHash[0] ^= 0xff
		input.ResponseBody[0] = '!'
		*input.ResourceID = 999
		return driver.RowsAffected(1), nil
	}
	script.idempotencyQuery = func(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
		return &idempotencyRecordRows{values: []driver.Value{int64(19), int64(7), input.Operation, input.IdempotencyKey, expectedHash[:], input.ResourceType, fmt.Sprint(expectedResource), int64(201), expectedBody, input.ExpiresAt, time.Now().UTC().Truncate(time.Microsecond)}}, nil
	}
	var saved *domain.IdempotencyRecord
	err := store.WithUserTransaction(context.Background(), actor, func(txContext context.Context, txStore *Store) error {
		var err error
		saved, err = txStore.CreateUserIdempotencyRecord(txContext, input)
		return err
	})
	if err != nil || saved == nil || saved.RequestHash != expectedHash || !bytes.Equal(saved.ResponseBody, expectedBody) || *saved.ResourceID != expectedResource {
		t.Fatal("输入修改影响已复制的保存字段", err)
	}
}
