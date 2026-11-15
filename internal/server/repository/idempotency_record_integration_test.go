package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestFindUserIdempotencyRecordIntegration(t *testing.T) {
	// 1. 复用独占库及用户夹具，安装既有正式幂等记录表。
	pool := loginTransactionDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE idempotency_records (")
	if start < 0 {
		t.Fatal("正式幂等表定义未找到")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:]); err != nil {
		t.Fatal(err)
	}
	hash, err := service.BuildCreateDirectoryRequestHash(11, "学习资料", nil)
	if err != nil {
		t.Fatal(err)
	}
	otherHash, err := service.BuildCreateDirectoryRequestHash(12, "其他资料", nil)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	expiresAt := stamp.Add(7 * 24 * time.Hour)
	operation := domain.CreateDirectoryOperation
	for _, record := range []struct {
		id, userID           uint64
		operation, key, kind string
		resourceID, body     any
		historical           bool
	}{
		{101, 1, operation, "Request-007", "ENTRY", uint64(23), `{"entry_id":"23","name":"学习资料"}`, false},
		{102, 3, operation, "Request-007", "ENTRY", uint64(33), `{"entry_id":"33"}`, false},
		{103, 1, "v1.other", "Request-007", "UPLOAD_TASK", uint64(43), `{"task_id":"43"}`, false},
		{104, 1, operation, "request-007", "ENTRY", uint64(24), `{"entry_id":"24"}`, false},
		{105, 1, strings.ToUpper(operation), "Request-007", "ENTRY", uint64(25), `{"entry_id":"25"}`, false},
		{106, 1, operation, "key'OR(1=1)--", "ENTRY", uint64(26), `{"entry_id":"26"}`, false},
		{107, 1, operation, "100%_", "ENTRY", uint64(27), `{"entry_id":"27"}`, false},
		{108, 2, operation, "historical", "FUTURE_RESOURCE", nil, `[]`, true},
		{110, 1, operation, "sql-null", "NONE", nil, nil, false},
		{111, 1, operation, "json-null", "NONE", nil, `null`, false},
		{112, 1, operation, "json-array", "NONE", nil, `[]`, false},
		{113, 1, operation, "json-scalar", "NONE", nil, `"legacy"`, false},
	} {
		created, expires := stamp, expiresAt
		if record.historical {
			created, expires = stamp.AddDate(-7, 0, 0), stamp.AddDate(-6, 0, 0)
		}
		digest := hash
		if record.id == 102 {
			digest = otherHash
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO idempotency_records(id,user_id,operation,idempotency_key,request_hash,resource_type,resource_id,response_status,response_body,expires_at,created_at) VALUES(?,?,?,?,?,?,?,201,?,?,?)",
			record.id, record.userID, record.operation, record.key, digest[:], record.kind, record.resourceID, record.body, expires, created); err != nil {
			t.Fatal("创建幂等查询夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	find := func(t *testing.T, reader *Store, readContext context.Context, userID uint64, operation, key string, recordID uint64) *domain.IdempotencyRecord {
		t.Helper()
		record, err := reader.FindUserIdempotencyRecord(readContext, userID, operation, key)
		if recordID == 0 {
			if record != nil || !errors.Is(err, domain.ErrIdempotencyRecordNotFound) {
				t.Fatal("缺失或其他范围记录的返回错误", err)
			}
			return nil
		}
		if err != nil || record == nil || record.ID != recordID || record.UserID != userID || record.Operation != operation || record.IdempotencyKey != key {
			t.Fatal("幂等记录标识或查询范围错误", err)
		}
		return record
	}

	// 2. 核对字段、独立副本、用户/操作/键范围以及 JSON 和历史事实。
	t.Run("fields copies and read only", func(t *testing.T) {
		var before int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM idempotency_records").Scan(&before); err != nil {
			t.Fatal(err)
		}
		first := find(t, store, ctx, 1, operation, "Request-007", 101)
		second := find(t, store, ctx, 1, operation, "Request-007", 101)
		var body map[string]string
		if first.RequestHash != hash || first.ResourceType != "ENTRY" || first.ResourceID == nil || *first.ResourceID != 23 || first.ResponseStatus != 201 || first.CreatedAt != stamp || first.ExpiresAt != expiresAt || json.Unmarshal(first.ResponseBody, &body) != nil || body["entry_id"] != "23" || body["name"] != "学习资料" {
			t.Fatal("十一项记录字段读回错误")
		}
		if !reflect.DeepEqual(first, second) || first == second || first.ResourceID == second.ResourceID {
			t.Fatal("查询结果未保持独立副本")
		}
		first.RequestHash[0] ^= 0xff
		*first.ResourceID = 999
		first.ResponseBody[0] = '!'
		if second.RequestHash != hash || *second.ResourceID != 23 || !json.Valid(second.ResponseBody) || !reflect.DeepEqual(find(t, store, ctx, 1, operation, "Request-007", 101), second) {
			t.Fatal("修改结果影响其他副本或数据库")
		}
		var after int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM idempotency_records").Scan(&after); err != nil || before != after {
			t.Fatal("读取修改了记录数量", err)
		}
	})
	t.Run("scope case and literal matching", func(t *testing.T) {
		if record := find(t, store, ctx, 3, operation, "Request-007", 102); record.RequestHash != otherHash || record.ResourceID == nil || *record.ResourceID != 33 {
			t.Fatal("不同用户的摘要或资源被混用")
		}
		find(t, store, ctx, 1, "v1.other", "Request-007", 103)
		find(t, store, ctx, 1, operation, "request-007", 104)
		find(t, store, ctx, 1, strings.ToUpper(operation), "Request-007", 105)
		find(t, store, ctx, 1, operation, "key'OR(1=1)--", 106)
		find(t, store, ctx, 1, operation, "100%_", 107)
		for _, key := range []string{"missing", "%", "_", "key'OR(1=2)--"} {
			find(t, store, ctx, 1, operation, key, 0)
		}
		find(t, store, ctx, 99, operation, "Request-007", 0)
		find(t, store, ctx, 1, "v2.other", "Request-007", 0)
	})
	t.Run("null json shapes and historical record", func(t *testing.T) {
		for _, item := range []struct {
			id   uint64
			key  string
			body []byte
		}{
			{110, "sql-null", nil}, {111, "json-null", []byte("null")},
			{112, "json-array", []byte("[]")}, {113, "json-scalar", []byte(`"legacy"`)},
		} {
			record := find(t, store, ctx, 1, operation, item.key, item.id)
			if record.ResourceID != nil || (record.ResponseBody == nil) != (item.body == nil) || !bytes.Equal(record.ResponseBody, item.body) {
				t.Fatal("SQL NULL、JSON null 或 JSON 形状丢失")
			}
		}
		if historical := find(t, store, ctx, 2, operation, "historical", 108); historical.ResourceType != "FUTURE_RESOURCE" || historical.ExpiresAt != stamp.AddDate(-6, 0, 0) || historical.CreatedAt != stamp.AddDate(-7, 0, 0) {
			t.Fatal("历史过期记录、资源类型或禁用用户事实丢失")
		}
	})
	t.Run("unique index lookup", func(t *testing.T) {
		var encoded []byte
		if err := pool.QueryRowContext(ctx, "EXPLAIN FORMAT=JSON SELECT id,user_id,operation,idempotency_key,request_hash,resource_type,resource_id,response_status,response_body,expires_at,created_at FROM idempotency_records WHERE user_id=? AND operation=? AND idempotency_key=? LIMIT 1", 1, operation, "Request-007").Scan(&encoded); err != nil {
			t.Fatal(err)
		}
		var plan struct {
			QueryBlock struct {
				Table struct {
					Key string `json:"key"`
				} `json:"table"`
			} `json:"query_block"`
		}
		if err := json.Unmarshal(encoded, &plan); err != nil || plan.QueryBlock.Table.Key != "uk_idempotency_request" {
			t.Fatal("查询未使用既有幂等唯一索引", err)
		}
	})

	// 3. 事务内复用同一连接，并验证未提交写入及明确的可重复读快照。
	t.Run("single connection user transaction", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		if err := store.WithLoginUserTransaction(ctx, 1, func(txContext context.Context, txStore *Store) error {
			find(t, txStore, txContext, 1, operation, "Request-007", 101)
			return nil
		}); err != nil {
			t.Fatal("事务内查询借用了其他连接", err)
		}
	})
	t.Run("consistent read under uncommitted result update", func(t *testing.T) {
		before := find(t, store, ctx, 1, operation, "Request-007", 101)
		writer, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		if _, err := writer.ExecContext(ctx, "UPDATE idempotency_records SET response_status=202,response_body=? WHERE id=101", `{"state":"pending"}`); err != nil {
			t.Fatal(err)
		}
		readContext, release := context.WithTimeout(ctx, 500*time.Millisecond)
		defer release()
		if current := find(t, store, readContext, 1, operation, "Request-007", 101); !reflect.DeepEqual(current, before) {
			t.Fatal("读取混入了未提交的结果字段")
		}
		if err := writer.Commit(); err != nil {
			t.Fatal(err)
		}
		if current := find(t, store, ctx, 1, operation, "Request-007", 101); current.ResponseStatus != 202 || reflect.DeepEqual(current, before) {
			t.Fatal("新查询未取得已提交结果")
		}
	})
	t.Run("repeatable read snapshot", func(t *testing.T) {
		if err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			before := find(t, txStore, txContext, 1, operation, "Request-007", 101)
			if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET response_status=201,response_body=? WHERE id=101", `{"state":"completed"}`); err != nil {
				return err
			}
			if current := find(t, txStore, txContext, 1, operation, "Request-007", 101); !reflect.DeepEqual(current, before) {
				t.Fatal("同一只读快照的幂等结果发生变化")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if current := find(t, store, ctx, 1, operation, "Request-007", 101); current.ResponseStatus != 201 {
			t.Fatal("快照结束后未取得新结果")
		}
	})

	// 4. 最大自增记录放在最后，核对完整整数、日期边界及数据库故障。
	t.Run("unsigned ids capacity and database dates", func(t *testing.T) {
		maximum := ^uint64(0)
		minimumDate := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
		maximumDate := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,password_hash,status) VALUES(?,'fixture_max','fixture_hash','ACTIVE')", maximum); err != nil {
			t.Fatal(err)
		}
		op, key := strings.Repeat("O", 64), strings.Repeat("K", 128)
		body := `{"resource_id":"18446744073709551615","value":18446744073709551615}`
		if _, err := pool.ExecContext(ctx, "INSERT INTO idempotency_records(id,user_id,operation,idempotency_key,request_hash,resource_type,resource_id,response_status,response_body,expires_at,created_at) VALUES(?,?,?,?,?,'ENTRY',?,599,?,?,?)", maximum, maximum, op, key, hash[:], maximum, body, maximumDate, minimumDate); err != nil {
			t.Fatal(err)
		}
		record := find(t, store, ctx, maximum, op, key, maximum)
		if record.ResourceID == nil || *record.ResourceID != maximum || record.RequestHash != hash || record.ResponseStatus != 599 || record.ExpiresAt != maximumDate || record.CreatedAt != minimumDate || !bytes.Contains(record.ResponseBody, []byte("18446744073709551615")) {
			t.Fatal("完整整数、日期、容量或 JSON 原文错误")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(record.ResponseBody, &fields); err != nil || string(fields["value"]) != "18446744073709551615" {
			t.Fatal("JSON 数字原文丢失完整整数精度", err)
		}
	})
	t.Run("closed pool keeps database error", func(t *testing.T) {
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		if record, err := store.FindUserIdempotencyRecord(ctx, 1, operation, "Request-007"); record != nil || err == nil || errors.Is(err, domain.ErrIdempotencyRecordNotFound) {
			t.Fatal("数据库故障被当作幂等记录缺失")
		}
	})
}
