package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type idempotencyRecordRows struct {
	values []driver.Value
	read   bool
	err    error
}

func (*idempotencyRecordRows) Columns() []string {
	return []string{"id", "user_id", "operation", "idempotency_key", "request_hash", "resource_type", "resource_id", "response_status", "response_body", "expires_at", "created_at"}
}
func (*idempotencyRecordRows) Close() error { return nil }
func (rows *idempotencyRecordRows) Next(destination []driver.Value) error {
	if rows.err != nil {
		return rows.err
	}
	if rows.read || rows.values == nil {
		return io.EOF
	}
	copy(destination, rows.values)
	rows.read = true
	return nil
}

func TestFindUserIdempotencyRecordResults(t *testing.T) {
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	resourceID := uint64(23)
	hash := sha256.Sum256([]byte("fixture request"))
	base := domain.IdempotencyRecord{
		ID: 19, UserID: 7, Operation: domain.CreateDirectoryOperation, IdempotencyKey: "Request-007", RequestHash: hash,
		ResourceType: "ENTRY", ResourceID: &resourceID, ResponseStatus: 201, ResponseBody: json.RawMessage(`{"entry_id":"23","name":"学习资料"}`),
		ExpiresAt: stamp.Add(7 * 24 * time.Hour), CreatedAt: stamp,
	}
	queryFailure := errors.New("fixture idempotency query failure")
	for _, testCase := range []struct {
		name     string
		column   int
		value    driver.Value
		queryErr error
		rowsErr  error
		wantErr  error
		missing  bool
	}{
		{name: "complete record", column: -1},
		{name: "sql null body and resource", column: -1},
		{name: "json null body", column: -1},
		{name: "json array body", column: -1},
		{name: "json scalar body", column: -1},
		{name: "expired record retained", column: -1},
		{name: "unknown resource type retained", column: -1},
		{name: "zero digest retained", column: -1},
		{name: "maximum unsigned ids", column: -1},
		{name: "input capacity", column: -1},
		{name: "sql punctuation", column: -1},
		{name: "minimum http status", column: -1},
		{name: "maximum http status", column: -1},
		{name: "missing", column: -1, missing: true, wantErr: domain.ErrIdempotencyRecordNotFound},
		{name: "wrong owner", column: 1, value: int64(8), wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "wrong operation", column: 2, value: "v1.other", wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "wrong key", column: 3, value: "request-007", wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "zero id", column: 0, value: int64(0), wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "short digest", column: 4, value: make([]byte, 31), wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "long digest", column: 4, value: make([]byte, 33), wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "nil digest", column: 4, value: nil, wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "empty resource type", column: 5, value: "", wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "zero resource id", column: 6, value: int64(0), wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "low status", column: 7, value: int64(99), wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "high status", column: 7, value: int64(600), wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "invalid json", column: 8, value: []byte(`{"private-body":`), wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "empty json", column: 8, value: []byte{}, wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "invalid json utf8", column: 8, value: []byte("\"private-body\xff\""), wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "zero expiry", column: 9, value: time.Time{}, wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "zero created time", column: 10, value: time.Time{}, wantErr: domain.ErrInvalidIdempotencyRecordData},
		{name: "invalid id scan", column: 0, value: "invalid-id"},
		{name: "negative owner scan", column: 1, value: int64(-1)},
		{name: "overflow resource scan", column: 6, value: "18446744073709551616"},
		{name: "invalid date scan", column: 9, value: "invalid-date"},
		{name: "query error", column: -1, queryErr: queryFailure, wantErr: queryFailure},
		{name: "row error", column: -1, missing: true, rowsErr: queryFailure, wantErr: queryFailure},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 准备独立投影值，检查十一列和三项范围条件的绑定。
			want := base
			values := []driver.Value{int64(19), int64(7), base.Operation, base.IdempotencyKey, hash[:], "ENTRY", int64(23), int64(201), []byte(base.ResponseBody), base.ExpiresAt, stamp}
			switch testCase.name {
			case "sql null body and resource":
				want.ResponseBody, want.ResourceID = nil, nil
				values[6], values[8] = nil, nil
			case "json null body":
				want.ResponseBody, values[8] = json.RawMessage("null"), []byte("null")
			case "json array body":
				want.ResponseBody, values[8] = json.RawMessage("[]"), []byte("[]")
			case "json scalar body":
				want.ResponseBody, values[8] = json.RawMessage(`"legacy"`), []byte(`"legacy"`)
			case "expired record retained":
				want.ExpiresAt = stamp.AddDate(-1, 0, 0)
				values[9] = want.ExpiresAt
			case "unknown resource type retained":
				want.ResourceType, values[5] = "FUTURE_RESOURCE", "FUTURE_RESOURCE"
			case "zero digest retained":
				want.RequestHash, values[4] = [sha256.Size]byte{}, make([]byte, sha256.Size)
			case "maximum unsigned ids":
				maximum := ^uint64(0)
				want.ID, want.UserID, want.ResourceID = maximum, maximum, &maximum
				values[0], values[1], values[6] = "18446744073709551615", "18446744073709551615", "18446744073709551615"
			case "input capacity":
				want.Operation, want.IdempotencyKey = strings.Repeat("A", 64), strings.Repeat("B", 128)
				values[2], values[3] = want.Operation, want.IdempotencyKey
			case "sql punctuation":
				want.Operation, want.IdempotencyKey = "v1.op'", "key'OR(1=1)--"
				values[2], values[3] = want.Operation, want.IdempotencyKey
			case "minimum http status":
				want.ResponseStatus, values[7] = 100, int64(100)
			case "maximum http status":
				want.ResponseStatus, values[7] = 599, int64(599)
			}
			if testCase.column >= 0 {
				values[testCase.column] = testCase.value
			}
			if testCase.missing {
				values = nil
			}
			queries := 0
			connector := &deviceConnector{accountConnector: accountConnector{query: func(ctx context.Context, statement string, arguments []driver.NamedValue) (driver.Rows, error) {
				queries++
				expected := "SELECT id,user_id,operation,idempotency_key,request_hash,resource_type,resource_id,response_status,response_body,expires_at,created_at FROM idempotency_records WHERE user_id = ? AND operation = ? AND idempotency_key = ? LIMIT ?"
				actual := strings.Join(strings.Fields(strings.ReplaceAll(statement, "`", "")), " ")
				if actual != expected || len(arguments) != 4 || arguments[0].Value != want.UserID || arguments[1].Value != want.Operation || arguments[2].Value != want.IdempotencyKey || arguments[3].Value != int64(1) {
					t.Error("幂等查询投影、范围或绑定参数错误")
				}
				if deadline, exists := ctx.Deadline(); !exists || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
					t.Error("幂等查询期限错误")
				}
				return &idempotencyRecordRows{values: values, err: testCase.rowsErr}, testCase.queryErr
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 核对事实、缺失和异常，查询保持共享连接池可用。
			record, err := store.FindUserIdempotencyRecord(context.Background(), want.UserID, want.Operation, want.IdempotencyKey)
			failed := testCase.wantErr != nil || strings.Contains(testCase.name, "scan")
			if failed {
				if record != nil || err == nil || (testCase.wantErr != nil && !errors.Is(err, testCase.wantErr)) {
					t.Fatal("幂等查询失败类别或空结果错误", err)
				}
			} else if err != nil || !reflect.DeepEqual(record, &want) {
				t.Fatal("幂等记录事实未完整读回", err)
			}
			if queries != 1 {
				t.Fatal("幂等查询未恰好执行一次")
			}
			if err := pool.PingContext(context.Background()); err != nil {
				t.Fatal("查询关闭了共享连接池", err)
			}
		})
	}
}

func TestFindUserIdempotencyRecordInput(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, release := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer release()
	connector := &deviceConnector{}
	pool := sql.OpenDB(connector)
	defer pool.Close()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		ctx            context.Context
		userID         uint64
		operation, key string
		want           error
	}{
		{userID: 7, operation: "v1.op", key: "key"},
		{ctx: context.Background(), operation: "v1.op", key: "key"},
		{ctx: context.Background(), userID: 7, key: "key"},
		{ctx: context.Background(), userID: 7, operation: strings.Repeat("a", 65), key: "key"},
		{ctx: context.Background(), userID: 7, operation: "v1.op"},
		{ctx: context.Background(), userID: 7, operation: "v1.op", key: strings.Repeat("a", 129)},
		{ctx: cancelled, userID: 7, operation: "v1.op", key: "key", want: context.Canceled},
		{ctx: expired, userID: 7, operation: "v1.op", key: "key", want: context.DeadlineExceeded},
	} {
		record, err := store.FindUserIdempotencyRecord(testCase.ctx, testCase.userID, testCase.operation, testCase.key)
		if record != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || connector.connections.Load() != 0 {
			t.Error("无效范围或预先取消仍访问数据库", err)
		}
	}
}

func TestFindUserIdempotencyRecordCancellation(t *testing.T) {
	for _, phase := range []string{"caller deadline", "default deadline", "after result", "after missing", "after failure"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "caller deadline" {
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 30*time.Millisecond)
				defer release()
			}
			connector := &deviceConnector{accountConnector: accountConnector{query: func(queryContext context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
				if strings.HasPrefix(phase, "after") {
					cancel()
					if phase == "after failure" {
						return nil, errors.New("fixture cancelled query")
					}
					if phase == "after result" {
						stamp := time.Now().UTC()
						return &idempotencyRecordRows{values: []driver.Value{int64(19), int64(7), "v1.op", "key", make([]byte, 32), "ENTRY", nil, int64(201), nil, stamp.Add(time.Hour), stamp}}, nil
					}
					return &idempotencyRecordRows{}, nil
				}
				<-queryContext.Done()
				return nil, queryContext.Err()
			}}}
			pool := sql.OpenDB(connector)
			defer pool.Close()
			store, err := NewStore(pool)
			if err != nil {
				t.Fatal(err)
			}
			record, err := store.FindUserIdempotencyRecord(ctx, 7, "v1.op", "key")
			want := error(context.DeadlineExceeded)
			if strings.HasPrefix(phase, "after") {
				want = context.Canceled
			}
			if record != nil || !errors.Is(err, want) {
				t.Fatal("查询取消未保留原因或仍返回记录", err)
			}
		})
	}
}
