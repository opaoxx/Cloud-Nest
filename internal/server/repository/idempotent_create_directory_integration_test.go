package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

type idempotentDirectorySaveFailureStore struct {
	*Store
	failure error
}

func (store *idempotentDirectorySaveFailureStore) CreateUserIdempotencyRecord(ctx context.Context, input domain.IdempotencyResultInput) (*domain.IdempotencyRecord, error) {
	if _, err := store.Store.CreateUserIdempotencyRecord(ctx, input); err != nil {
		return nil, err
	}
	return nil, store.failure
}

func installDirectoryChangeEventTable(t *testing.T, ctx context.Context, pool *sql.DB) {
	t.Helper()
	// 与现有部分迁移夹具一致，只安装本用例直接使用的正式表。
	changeEventsSQL, _, found := strings.Cut(migrations.Ordered()[2].SQL, ";\n")
	if !found || !strings.Contains(changeEventsSQL, "CREATE TABLE change_events (") {
		t.Fatal("正式变更事件表边界错误")
	}
	if _, err := pool.ExecContext(ctx, changeEventsSQL); err != nil {
		t.Fatal(err)
	}
}

func TestIdempotentCreateDirectoryServiceIntegration(t *testing.T) {
	// 1. 使用正式表独占库和实际用户事务，绑定既有 DTO 编解码。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	installDirectoryChangeEventTable(t, ctx, pool)
	sessionStart := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	sessionEnd := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	idempotencyStart := strings.Index(migrations.InitialSQL, "CREATE TABLE idempotency_records (")
	if sessionStart < 0 || sessionEnd <= sessionStart || idempotencyStart < 0 {
		t.Fatal("正式迁移边界错误")
	}
	for _, statement := range []string{
		migrations.InitialSQL[sessionStart:sessionEnd], migrations.InitialSQL[idempotencyStart:],
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','幂等创建用户','fixture_hash','ACTIVE')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','幂等创建设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
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
	withTransaction := func(ctx context.Context, current domain.Actor, action func(context.Context, service.IdempotentCreateDirectoryTransaction) error) error {
		return store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
	}
	encode := func(entry *domain.FileEntry, color *string) (json.RawMessage, error) {
		data, err := dto.NewCreateDirectoryData(entry, color)
		if err != nil {
			return nil, err
		}
		return json.Marshal(data)
	}
	validate := func(body json.RawMessage) error { _, err := dto.RestoreCreateDirectoryData(body); return err }
	creator := service.NewIdempotentCreateDirectoryService(withTransaction, encode, validate)
	counts := func() [4]int {
		t.Helper()
		var result [4]int
		if err := pool.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM file_entries),(SELECT COUNT(*) FROM idempotency_records),"+
			"(SELECT COUNT(*) FROM change_events WHERE user_id=1),change_seq FROM users WHERE id=1").
			Scan(&result[0], &result[1], &result[2], &result[3]); err != nil {
			t.Fatal(err)
		}
		return result
	}
	create := func(t *testing.T, name string, color *string, key string) *dto.CreateDirectoryData {
		t.Helper()
		body, err := creator.Create(ctx, actor, 11, name, color, key)
		if err != nil {
			t.Fatal("幂等创建失败", err)
		}
		data, err := dto.RestoreCreateDirectoryData(body)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	// 2. 首次创建与重试复用同一连接，目录变化后仍返回创建时快照。
	t.Run("single connection create replay and saved snapshot", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		green := "green"
		first := create(t, "  Cafe\u0301  ", &green, "create-007")
		record, err := store.FindUserIdempotencyRecord(ctx, 1, domain.CreateDirectoryOperation, "create-007")
		if err != nil || record.ResourceID == nil || fmt.Sprint(*record.ResourceID) != first.Entry.ID || record.ExpiresAt.Sub(record.CreatedAt) < 7*24*time.Hour {
			t.Fatal("结果关联或至少七天保留期错误", err)
		}
		before := counts()
		newKey, err := service.BuildEntryNameKey("后续改名")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name='后续改名',name_key=?,color_key='sand',revision=2 WHERE id=? AND user_id=1", newKey, *record.ResourceID); err != nil {
			t.Fatal(err)
		}
		retry := create(t, "Cafe\u0301", &green, "create-007")
		if retry.Entry.ID != first.Entry.ID || retry.Entry.Name != "Cafe\u0301" || retry.Entry.Revision != "1" || *retry.Entry.ColorKey != "green" || counts() != before {
			t.Fatal("重试未保留创建快照或重复写入")
		}
	})
	t.Run("request conflict and distinct keys", func(t *testing.T) {
		before := counts()
		if body, err := creator.Create(ctx, actor, 11, "不同参数", nil, "create-007"); body != nil || !errors.Is(err, domain.ErrIdempotencyRequestConflict) || counts() != before {
			t.Fatal("同键不同参数未拒绝或产生新数据", err)
		}
		create(t, "独立请求", nil, "new-key")
		before = counts()
		if body, err := creator.Create(ctx, actor, 11, "独立请求", nil, "another-key"); body != nil || !errors.Is(err, domain.ErrEntryNameConflict) || counts() != before {
			t.Fatal("不同键重名未按目录规则拒绝", err)
		}
	})
	t.Run("optional colors", func(t *testing.T) {
		empty, green := "", "green"
		for index, color := range []*string{nil, &empty, &green} {
			name, key := fmt.Sprintf("颜色-%d", index), fmt.Sprintf("color-%d", index)
			data := create(t, name, color, key)
			if (data.Entry.ColorKey == nil) != (color == nil) || (color != nil && *data.Entry.ColorKey != *color) {
				t.Fatal("可空颜色未准确保存")
			}
			create(t, name, color, key)
		}
	})

	// 3. 真实写入之后注入故障，核对目录与结果共同回滚。
	t.Run("atomic rollback", func(t *testing.T) {
		failure := errors.New("fixture atomic rollback")
		for _, mode := range []string{"encode", "save after insert", "outer callback", "cancel after encode", "validate after save"} {
			before := counts()
			writeContext, cancelWrite := context.WithCancel(ctx)
			encodeResult, validateResult := encode, validate
			transaction := withTransaction
			switch mode {
			case "encode":
				encodeResult = func(*domain.FileEntry, *string) (json.RawMessage, error) { return nil, failure }
			case "save after insert", "outer callback":
				transaction = func(ctx context.Context, current domain.Actor, action func(context.Context, service.IdempotentCreateDirectoryTransaction) error) error {
					return store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
						if mode == "save after insert" {
							return action(txContext, &idempotentDirectorySaveFailureStore{Store: txStore, failure: failure})
						}
						if err := action(txContext, txStore); err != nil {
							return err
						}
						return failure
					})
				}
			case "cancel after encode":
				encodeResult = func(entry *domain.FileEntry, color *string) (json.RawMessage, error) {
					body, err := encode(entry, color)
					cancelWrite()
					return body, err
				}
			case "validate after save":
				checks := 0
				validateResult = func(body json.RawMessage) error {
					checks++
					if checks == 2 {
						return failure
					}
					return validate(body)
				}
			}
			failing := service.NewIdempotentCreateDirectoryService(transaction, encodeResult, validateResult)
			body, err := failing.Create(writeContext, actor, 11, "回滚-"+mode, nil, "rollback-"+strings.ReplaceAll(mode, " ", "-"))
			cancelWrite()
			want := failure
			if mode == "cancel after encode" {
				want = context.Canceled
			}
			if body != nil || !errors.Is(err, want) || counts() != before {
				t.Fatal("失败未回滚目录、幂等快照、事件或序号，或未保留原因", mode, err)
			}
		}
	})
	t.Run("identity recheck on replay", func(t *testing.T) {
		for _, mutation := range []struct {
			change, restore string
			want            error
		}{
			{"UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
			{"UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=13", "UPDATE user_devices SET revoked_at=NULL WHERE id=13", domain.ErrDeviceRevoked},
			{"UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		} {
			if _, err := pool.ExecContext(ctx, mutation.change); err != nil {
				t.Fatal(err)
			}
			body, callError := creator.Create(ctx, actor, 11, "独立请求", nil, "new-key")
			if _, err := pool.ExecContext(ctx, mutation.restore); err != nil {
				t.Fatal(err)
			}
			if body != nil || !errors.Is(callError, mutation.want) {
				t.Fatal("重试绕过当前身份复查", callError)
			}
		}
	})

	// 4. 同键并发在用户锁内串行判断，同参数得到同一结果，不同参数得到冲突。
	t.Run("same key concurrency", func(t *testing.T) {
		for _, different := range []bool{false, true} {
			before := counts()
			key := fmt.Sprintf("concurrent-%t", different)
			type outcome struct {
				body json.RawMessage
				err  error
			}
			results := make(chan outcome, 2)
			start := make(chan struct{})
			for index := 0; index < 2; index++ {
				name := "并发-" + key
				if different {
					name += fmt.Sprint(index)
				}
				go func() {
					<-start
					body, err := creator.Create(ctx, actor, 11, name, nil, key)
					results <- outcome{body, err}
				}()
			}
			close(start)
			first, second := <-results, <-results
			if !different && (first.err != nil || second.err != nil || !bytes.Equal(first.body, second.body)) {
				t.Fatal("同键同参数并发未返回同一结果", first.err, second.err)
			}
			if different && !((first.err == nil && errors.Is(second.err, domain.ErrIdempotencyRequestConflict)) || (second.err == nil && errors.Is(first.err, domain.ErrIdempotencyRequestConflict))) {
				t.Fatal("同键不同参数并发未得到一次成功和一次冲突", first.err, second.err)
			}
			after := counts()
			if after != [4]int{before[0] + 1, before[1] + 1, before[2] + 1, before[3] + 1} {
				t.Fatal("并发写入数量错误", before, after)
			}
		}
	})
	t.Run("expired and corrupt saved result", func(t *testing.T) {
		create(t, "过期结果", nil, "expired-key")
		before := counts()
		// 历史区间确保容器与主机时钟存在秒级偏差时，夹具依然已经到期。
		if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET created_at=UTC_TIMESTAMP(6)-INTERVAL 9 DAY,expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 DAY WHERE idempotency_key='expired-key' AND user_id=1"); err != nil {
			t.Fatal(err)
		}
		expired, err := store.FindUserIdempotencyRecord(ctx, 1, domain.CreateDirectoryOperation, "expired-key")
		if err != nil || !expired.ExpiresAt.Before(time.Now()) {
			t.Fatal("到期夹具未处于服务时钟的历史区间", err)
		}
		if body, err := creator.Create(ctx, actor, 11, "过期结果", nil, "expired-key"); body != nil || !errors.Is(err, domain.ErrIdempotencyRecordExpired) || counts() != before {
			t.Fatal("过期记录未按既定边界返回", err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=JSON_OBJECT('entry',JSON_OBJECT('id',CAST(resource_id AS CHAR))) WHERE idempotency_key='new-key' AND user_id=1"); err != nil {
			t.Fatal(err)
		}
		if body, err := creator.Create(ctx, actor, 11, "独立请求", nil, "new-key"); body != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || counts() != before {
			t.Fatal("异常完整快照未被事务内校验拒绝", err)
		}
	})
	t.Run("high unsigned generated ids", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "ALTER TABLE file_entries AUTO_INCREMENT=9223372036854776000"); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "ALTER TABLE idempotency_records AUTO_INCREMENT=9223372036854776000"); err != nil {
			t.Fatal(err)
		}
		data := create(t, "高位目录", nil, "high-id")
		if data.Entry.ID != "9223372036854776000" {
			t.Fatal("高位目录 ID 丢失")
		}
		record, err := store.FindUserIdempotencyRecord(ctx, 1, domain.CreateDirectoryOperation, "high-id")
		if err != nil || record.ID != 9223372036854776000 {
			t.Fatal("高位幂等记录 ID 丢失", err)
		}
		create(t, "高位目录", nil, "high-id")
	})
}
