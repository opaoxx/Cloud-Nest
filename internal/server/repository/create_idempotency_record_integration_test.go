package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestCreateUserIdempotencyRecordIntegration(t *testing.T) {
	// 1. 在正式目录独占库中安装会话及幂等表，准备已认证事务身份。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	sessionStart := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	sessionEnd := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	idempotencyStart := strings.Index(migrations.InitialSQL, "CREATE TABLE idempotency_records (")
	if sessionStart < 0 || sessionEnd <= sessionStart || idempotencyStart < 0 {
		t.Fatal("正式迁移边界错误")
	}
	for _, statement := range []string{
		migrations.InitialSQL[sessionStart:sessionEnd], migrations.InitialSQL[idempotencyStart:],
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','幂等结果用户','fixture_hash','ACTIVE'),(3,'fixture_three','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','结果保存设备','WEB'),(31,3,'38ee758a-0727-42de-ae7d-3bb33155e148','其他用户设备','WINDOWS')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),(37,3,31,UNHEX(REPEAT('33',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 13, SessionID: 17}
	newInput := func(key string) domain.IdempotencyResultInput {
		input := idempotencyResultFixture()
		input.Operation, input.IdempotencyKey = "v1.result.probe", key
		input.ResourceType, input.ResourceID = "NONE", nil
		input.ResponseBody = json.RawMessage(`{"ok":true,"name":"学习资料"}`)
		return input
	}
	save := func(saveContext context.Context, current domain.Actor, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error) {
		var prepared *domain.IdempotencyRecord
		err := store.WithUserTransaction(saveContext, current, func(txContext context.Context, txStore *Store) error {
			var err error
			prepared, err = txStore.CreateUserIdempotencyRecord(txContext, input)
			return err
		})
		if err != nil {
			return nil, err
		}
		return prepared, nil
	}
	find := func(t *testing.T, userID uint64, operation, key string) *domain.IdempotencyRecord {
		t.Helper()
		record, err := store.FindUserIdempotencyRecord(ctx, userID, operation, key)
		if err != nil || record == nil {
			t.Fatal("已保存结果查询失败", err)
		}
		return record
	}
	missing := func(t *testing.T, operation, key string) {
		t.Helper()
		if record, err := store.FindUserIdempotencyRecord(ctx, 1, operation, key); record != nil || !errors.Is(err, domain.ErrIdempotencyRecordNotFound) {
			t.Fatal("失败事务留下了幂等记录", err)
		}
	}

	// 2. 单连接保存、JSON 空值和重复范围采用实际数据库字段核对。
	t.Run("single connection defaults and utc expiry", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		input := newInput("Request-007")
		input.ExpiresAt = input.ExpiresAt.In(time.FixedZone("fixture", 8*3600)).Add(789 * time.Nanosecond)
		saved, err := save(ctx, actor, input)
		if err != nil || saved == nil || saved.ID == 0 || saved.UserID != 1 || saved.RequestHash != input.RequestHash || saved.ResourceID != nil || saved.ResponseStatus != 201 || !saved.ExpiresAt.Equal(input.ExpiresAt.UTC().Truncate(time.Microsecond)) || saved.ExpiresAt.Location() != time.UTC || saved.CreatedAt.IsZero() || saved.CreatedAt.Nanosecond()%1000 != 0 {
			t.Fatal("结果字段、归属或 UTC 微秒时间错误", err)
		}
		var body map[string]json.RawMessage
		if json.Unmarshal(saved.ResponseBody, &body) != nil || string(body["ok"]) != "true" || string(body["name"]) != `"学习资料"` {
			t.Fatal("JSON 文本未按内容保存")
		}
		input.ResponseBody[0] = '!'
		saved.RequestHash[0] ^= 0xff
		saved.ResponseBody[0] = '!'
		if actual := find(t, 1, input.Operation, input.IdempotencyKey); actual.RequestHash != input.RequestHash || !json.Valid(actual.ResponseBody) {
			t.Fatal("本地修改影响已保存记录")
		}
	})
	t.Run("occupied keys scopes and retained result", func(t *testing.T) {
		input := newInput("Request-007")
		original := find(t, 1, input.Operation, input.IdempotencyKey)
		input.RequestHash[0] ^= 0xff
		input.ResponseBody = json.RawMessage(`{"changed":true}`)
		if result, err := save(ctx, actor, input); result != nil || !errors.Is(err, domain.ErrIdempotencyRecordAlreadyExists) {
			t.Fatal("重复范围未返回专用存在错误", err)
		}
		if !reflect.DeepEqual(find(t, 1, input.Operation, input.IdempotencyKey), original) {
			t.Fatal("重复保存覆盖了原结果或保留时间")
		}
		other := domain.Actor{UserID: 3, DeviceID: 31, SessionID: 37}
		if record, err := save(ctx, other, input); err != nil || record.UserID != 3 {
			t.Fatal("其他用户的独立范围保存失败", err)
		}
		input.Operation = "v2.result.probe"
		if _, err := save(ctx, actor, input); err != nil {
			t.Fatal("其他操作范围保存失败", err)
		}
		input = newInput("request-007")
		if _, err := save(ctx, actor, input); err != nil {
			t.Fatal("大小写不同的键保存失败", err)
		}
		input = newInput("expired-occupied")
		if _, err := save(ctx, actor, input); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE user_id=1 AND operation=? AND idempotency_key=?", input.Operation, input.IdempotencyKey); err != nil {
			t.Fatal(err)
		}
		if record, err := save(ctx, actor, input); record != nil || !errors.Is(err, domain.ErrIdempotencyRecordAlreadyExists) {
			t.Fatal("已有历史记录未保留占用事实", err)
		}
	})
	t.Run("sql null json null and large numbers", func(t *testing.T) {
		for index, body := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage("[]"), json.RawMessage(`"legacy"`), json.RawMessage(`{"value":18446744073709551615}`)} {
			input := newInput(fmt.Sprintf("body-%d", index))
			input.ResponseBody = body
			if index == 0 {
				input.ResponseStatus = 100
			}
			if index == 4 {
				input.ResponseStatus = 599
			}
			saved, err := save(ctx, actor, input)
			if err != nil || saved == nil || saved.ResponseStatus != input.ResponseStatus || (saved.ResponseBody == nil) != (body == nil) {
				t.Fatal("JSON 空值或状态码边界保存错误", err)
			}
			if index > 0 && index < 4 && !bytes.Equal(saved.ResponseBody, body) {
				t.Fatal("JSON null、数组或标量含义改变")
			}
			if index == 4 {
				var fields map[string]json.RawMessage
				if json.Unmarshal(saved.ResponseBody, &fields) != nil || string(fields["value"]) != "18446744073709551615" {
					t.Fatal("JSON 数字原文精度丢失")
				}
			}
		}
	})

	// 3. 实际目录写入和结果保存复用同一事务，验证提交与各方向失败的原子性。
	t.Run("directory and result atomicity", func(t *testing.T) {
		outerFailure := errors.New("fixture outer failure")
		for index, mode := range []string{"commit", "invalid result", "duplicate", "outer failure"} {
			name := fmt.Sprintf("原子目录-%d", index)
			key, err := service.BuildEntryNameKey(name)
			if err != nil {
				t.Fatal(err)
			}
			input := newInput(fmt.Sprintf("atomic-%d", index))
			input.Operation = domain.CreateDirectoryOperation
			if mode == "duplicate" {
				input.Operation, input.IdempotencyKey = "v1.result.probe", "Request-007"
			}
			err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				entry, err := txStore.CreateUserDirectory(txContext, 11, name, key, nil)
				if err != nil {
					return err
				}
				input.RequestHash, err = service.BuildCreateDirectoryRequestHash(11, name, nil)
				if err != nil {
					return err
				}
				input.ResourceType, input.ResourceID = "ENTRY", &entry.ID
				input.ResponseBody, err = json.Marshal(map[string]string{"entry_id": fmt.Sprint(entry.ID), "name": entry.Name})
				if err != nil {
					return err
				}
				if mode == "invalid result" {
					input.ResponseBody = json.RawMessage(`{"invalid":`)
				}
				if _, err := txStore.CreateUserIdempotencyRecord(txContext, input); err != nil {
					return err
				}
				if mode == "outer failure" {
					return outerFailure
				}
				return nil
			})
			if mode == "commit" {
				if err != nil {
					t.Fatal("两类写入提交失败", err)
				}
				entry, err := store.FindUserActiveFileEntryByName(ctx, 1, 11, key)
				stored := find(t, 1, input.Operation, input.IdempotencyKey)
				if err != nil || entry == nil || stored.ResourceID == nil || *stored.ResourceID != entry.ID || stored.RequestHash != input.RequestHash {
					t.Fatal("提交后的目录和结果引用错误", err)
				}
				continue
			}
			want := domain.ErrInvalidIdempotencyRecordData
			if mode == "duplicate" {
				want = domain.ErrIdempotencyRecordAlreadyExists
			}
			if mode == "outer failure" {
				want = outerFailure
			}
			if !errors.Is(err, want) {
				t.Fatal("原子事务未保留失败原因", err)
			}
			if entry, err := store.FindUserActiveFileEntryByName(ctx, 1, 11, key); entry != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
				t.Fatal("失败事务留下了文件夹", err)
			}
			if mode != "duplicate" {
				missing(t, input.Operation, input.IdempotencyKey)
			}
		}
	})
	t.Run("cancellation and ended transaction", func(t *testing.T) {
		operationContext, stop := context.WithCancel(ctx)
		defer stop()
		input := newInput("cancelled")
		err := store.WithUserTransaction(operationContext, actor, func(txContext context.Context, txStore *Store) error {
			if _, err := txStore.CreateUserIdempotencyRecord(txContext, input); err != nil {
				return err
			}
			stop()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatal("提交前取消未返回取消错误", err)
		}
		missing(t, input.Operation, input.IdempotencyKey)
		var ended *Store
		if err := store.WithUserTransaction(ctx, actor, func(_ context.Context, txStore *Store) error {
			ended = txStore
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if record, err := ended.CreateUserIdempotencyRecord(ctx, newInput("after-end")); record != nil || !errors.Is(err, sql.ErrTxDone) {
			t.Fatal("已结束事务仍允许保存结果", err)
		}
	})

	// 4. 当前身份状态在事务入口复查，同用户同键并发通过唯一约束确定占用。
	t.Run("current authorization state", func(t *testing.T) {
		for _, testCase := range []struct {
			name, change, restore string
			want                  error
		}{
			{"disabled", "UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
			{"device revoked", "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=13", "UPDATE user_devices SET revoked_at=NULL WHERE id=13", domain.ErrDeviceRevoked},
			{"session revoked", "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
			{"session expired", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=17", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=17", domain.ErrSessionExpired},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				if _, err := pool.ExecContext(ctx, testCase.change); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := pool.ExecContext(ctx, testCase.restore); err != nil {
						t.Error(err)
					}
				}()
				if result, err := save(ctx, actor, newInput("auth-rejected")); result != nil || !errors.Is(err, testCase.want) {
					t.Fatal("失效身份仍保存结果", err)
				}
			})
		}
		missing(t, "v1.result.probe", "auth-rejected")
	})
	t.Run("concurrent occupied key", func(t *testing.T) {
		begin := make(chan struct{})
		results := make(chan error, 2)
		var workers sync.WaitGroup
		for range 2 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-begin
				_, err := save(ctx, actor, newInput("concurrent"))
				results <- err
			}()
		}
		close(begin)
		workers.Wait()
		close(results)
		succeeded, occupied := 0, 0
		for err := range results {
			if err == nil {
				succeeded++
			} else if errors.Is(err, domain.ErrIdempotencyRecordAlreadyExists) {
				occupied++
			} else {
				t.Fatal("同键并发返回异常", err)
			}
		}
		if succeeded != 1 || occupied != 1 {
			t.Fatal("同键并发未得到一次成功和一次占用")
		}
	})

	// 5. 最后安装高位身份和自增值，验证可空无符号资源参数及实际生成 ID。
	t.Run("unsigned generated ids and capacity", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','完整整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'48ee758a-0727-42de-ae7d-3bb33155e148','完整整数设备','WINDOWS')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "ALTER TABLE idempotency_records AUTO_INCREMENT = 9223372036854775810"); err != nil {
			t.Fatal(err)
		}
		input := newInput(strings.Repeat("K", 128))
		input.Operation, input.ResourceType = strings.Repeat("O", 64), strings.Repeat("R", 32)
		input.ResourceID, input.ResponseStatus = &maximum, 599
		input.ResponseBody = json.RawMessage(`{"value":18446744073709551615}`)
		input.ExpiresAt = time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		fullActor := domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}
		saved, err := save(ctx, fullActor, input)
		if err != nil || saved == nil || saved.ID < uint64(1)<<63 || saved.UserID != maximum || saved.ResourceID == nil || *saved.ResourceID != maximum || saved.ResponseStatus != 599 || !saved.ExpiresAt.Equal(input.ExpiresAt) {
			t.Fatal("完整整数、字段容量或截止时间保存失败", err)
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(saved.ResponseBody, &fields) != nil || string(fields["value"]) != "18446744073709551615" {
			t.Fatal("保存结果丢失 JSON 整数精度")
		}
	})
}
