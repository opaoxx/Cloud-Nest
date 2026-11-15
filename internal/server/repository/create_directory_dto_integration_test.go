package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestCreateDirectoryDataIntegration(t *testing.T) {
	// 1. 沿用正式目录独占库，补齐当前身份及结果保存依赖。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	sessionStart := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	sessionEnd := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	idempotencyStart := strings.Index(migrations.InitialSQL, "CREATE TABLE idempotency_records (")
	if sessionStart < 0 || sessionEnd <= sessionStart || idempotencyStart < 0 {
		t.Fatal("正式迁移边界错误")
	}
	for _, statement := range []string{
		migrations.InitialSQL[sessionStart:sessionEnd], migrations.InitialSQL[idempotencyStart:],
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','创建响应用户','fixture_hash','ACTIVE')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','响应测试设备','WEB')",
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
	createSnapshot := func(t *testing.T, name string, color *string, key string) (*dto.CreateDirectoryData, *domain.IdempotencyRecord) {
		t.Helper()
		nameKey, err := service.BuildEntryNameKey(name)
		if err != nil {
			t.Fatal(err)
		}
		hash, err := service.BuildCreateDirectoryRequestHash(11, name, color)
		if err != nil {
			t.Fatal(err)
		}
		var data *dto.CreateDirectoryData
		var saved *domain.IdempotencyRecord
		err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			entry, err := txStore.CreateUserDirectory(txContext, 11, name, nameKey, color)
			if err != nil {
				return err
			}
			display, err := txStore.FindUserFileEntryDisplay(txContext, actor.UserID, entry.ID)
			if err != nil {
				return err
			}
			data, err = dto.NewCreateDirectoryData(entry, display.ColorKey)
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				return err
			}
			saved, err = txStore.CreateUserIdempotencyRecord(txContext, domain.IdempotencyResultInput{
				Operation: domain.CreateDirectoryOperation, IdempotencyKey: key, RequestHash: hash,
				ResourceType: "ENTRY", ResourceID: &entry.ID, ResponseStatus: 201, ResponseBody: encoded,
				ExpiresAt: time.Now().Add(8 * 24 * time.Hour),
			})
			return err
		})
		if err != nil || data == nil || saved == nil {
			t.Fatal("创建响应快照或保存幂等结果失败", err)
		}
		return data, saved
	}

	// 2. 在同一事务内构造稳定数据并保存，提交后按记录 JSON 读回。
	t.Run("single connection stable data round trip", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(2)
		color := "green"
		data, saved := createSnapshot(t, "Cafe\u0301 🌱", &color, "snapshot-green")
		if saved.ResourceID == nil || data.Entry.ID != fmt.Sprint(*saved.ResourceID) || data.Entry.ParentID != "11" || data.Entry.Name != "Cafe\u0301 🌱" || data.Entry.Revision != "1" || data.Entry.ColorKey == nil || *data.Entry.ColorKey != "green" {
			t.Fatal("响应与实际保存结果不一致")
		}
		restored, err := dto.RestoreCreateDirectoryData(saved.ResponseBody)
		if err != nil || !reflect.DeepEqual(data, restored) {
			t.Fatal("数据库 JSON 读回未恢复创建时 DTO")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(saved.ResponseBody, &fields) != nil || len(fields) != 1 || fields["entry"] == nil {
			t.Fatal("稳定结果数据结构错误")
		}
		if data.Entry.CreatedAt.Location() != time.UTC || data.Entry.CreatedAt.Nanosecond()%1000 != 0 || !data.Entry.CreatedAt.Equal(data.Entry.ModifiedAt) || !data.Entry.CreatedAt.Equal(data.Entry.UpdatedAt) {
			t.Fatal("实际创建时间未按 UTC 微秒转换")
		}
	})
	t.Run("null and empty colors", func(t *testing.T) {
		empty := ""
		for index, color := range []*string{nil, &empty} {
			data, saved := createSnapshot(t, fmt.Sprintf("颜色目录-%d", index), color, fmt.Sprintf("snapshot-color-%d", index))
			restored, err := dto.RestoreCreateDirectoryData(saved.ResponseBody)
			if err != nil || !reflect.DeepEqual(data, restored) || (restored.Entry.ColorKey == nil) != (color == nil) || (color != nil && *restored.Entry.ColorKey != "") {
				t.Fatal("颜色 NULL 或空串在持久化后改变")
			}
		}
	})

	// 3. 当前目录发生变化后，幂等数据继续描述创建时快照，外层关联各自请求 ID。
	t.Run("created snapshot across later changes", func(t *testing.T) {
		green := "green"
		data, saved := createSnapshot(t, "原始名称", &green, "snapshot-change")
		newKey, err := service.BuildEntryNameKey("当前名称")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name='当前名称',name_key=?,color_key='sand',revision=revision+1,modified_at=UTC_TIMESTAMP(6),updated_at=UTC_TIMESTAMP(6) WHERE user_id=1 AND id=?", newKey, *saved.ResourceID); err != nil {
			t.Fatal(err)
		}
		current, err := store.FindUserFileEntry(ctx, 1, *saved.ResourceID)
		if err != nil || current.Name != "当前名称" || current.Revision != 2 {
			t.Fatal("当前目录变更夹具错误", err)
		}
		record, err := store.FindUserIdempotencyRecord(ctx, 1, domain.CreateDirectoryOperation, "snapshot-change")
		if err != nil {
			t.Fatal(err)
		}
		restored, err := dto.RestoreCreateDirectoryData(record.ResponseBody)
		if err != nil || !reflect.DeepEqual(data, restored) || restored.Entry.Name != "原始名称" || restored.Entry.Revision != "1" || restored.Entry.ColorKey == nil || *restored.Entry.ColorKey != "green" {
			t.Fatal("创建快照混入当前目录字段")
		}
		first := dto.CreateDirectoryResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}
		next := dto.CreateDirectoryResponse{Data: *restored, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e149"}
		if !reflect.DeepEqual(first.Data, next.Data) || first.RequestID == next.RequestID {
			t.Fatal("稳定数据与本次请求 ID 关联错误")
		}
		if _, err := json.Marshal(next); err != nil {
			t.Fatal("已保存结果无法组成响应", err)
		}
	})
	// 4. 实际保存后，在用户事务内比对当前请求并恢复原始快照。
	t.Run("replay validation and request conflicts", func(t *testing.T) {
		data, saved := createSnapshot(t, "复用资料", nil, "snapshot-replay")
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(2)
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			record, err := txStore.FindUserIdempotencyRecord(txContext, actor.UserID, domain.CreateDirectoryOperation, "snapshot-replay")
			if err != nil {
				return err
			}
			for _, name := range []string{"复用资料", " 复用资料\u3000", "其他资料"} {
				hash, err := service.BuildCreateDirectoryRequestHash(11, name, nil)
				if err != nil {
					return err
				}
				validationError := service.ValidateCreateDirectoryReplay(record, actor.UserID, "snapshot-replay", hash, time.Now())
				if name == "其他资料" {
					if !errors.Is(validationError, domain.ErrIdempotencyRequestConflict) {
						return fmt.Errorf("同键不同参数未识别冲突: %w", validationError)
					}
					continue
				}
				if validationError != nil {
					return validationError
				}
				restored, err := dto.RestoreCreateDirectoryData(record.ResponseBody)
				if err != nil || !reflect.DeepEqual(data, restored) {
					return fmt.Errorf("复用校验后未准确恢复创建快照: %w", err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		reloaded, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.CreateDirectoryOperation, "snapshot-replay")
		if err != nil || !reflect.DeepEqual(saved, reloaded) {
			t.Fatal("复用判断修改了已保存结果", err)
		}
	})
	t.Run("stored replay boundaries", func(t *testing.T) {
		_, saved := createSnapshot(t, "复用边界", nil, "snapshot-replay-boundary")
		if err := service.ValidateCreateDirectoryReplay(saved, actor.UserID, saved.IdempotencyKey, saved.RequestHash, saved.ExpiresAt.Add(-time.Nanosecond)); err != nil {
			t.Fatal("截止前记录未通过", err)
		}
		if err := service.ValidateCreateDirectoryReplay(saved, actor.UserID, saved.IdempotencyKey, saved.RequestHash, saved.ExpiresAt); !errors.Is(err, domain.ErrIdempotencyRecordExpired) {
			t.Fatal("到期记录未返回专用错误", err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=JSON_SET(response_body,'$.entry.id','11') WHERE id=? AND user_id=1", saved.ID); err != nil {
			t.Fatal(err)
		}
		record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.CreateDirectoryOperation, saved.IdempotencyKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.ValidateCreateDirectoryReplay(record, actor.UserID, record.IdempotencyKey, record.RequestHash, time.Now()); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
			t.Fatal("正文目录 ID 与元数据资源 ID 不一致未拒绝", err)
		}
	})

	// 5. 数据库允许合法 JSON，恢复层继续检查创建结果的结构和字段。
	t.Run("invalid saved creation data", func(t *testing.T) {
		_, saved := createSnapshot(t, "异常结果恢复", nil, "snapshot-invalid")
		var payload map[string]map[string]json.RawMessage
		if err := json.Unmarshal(saved.ResponseBody, &payload); err != nil {
			t.Fatal(err)
		}
		delete(payload["entry"], "size_bytes")
		missingSize, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range []string{"null", "[]", `{"entry":null}`, string(missingSize)} {
			if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=? WHERE id=? AND user_id=1", body, saved.ID); err != nil {
				t.Fatal(err)
			}
			record, err := store.FindUserIdempotencyRecord(ctx, 1, domain.CreateDirectoryOperation, "snapshot-invalid")
			if err != nil {
				t.Fatal("合法 JSON 事实读取失败", err)
			}
			if data, err := dto.RestoreCreateDirectoryData(record.ResponseBody); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatal("异常保存结果未被恢复层拒绝", err)
			}
		}
	})
	t.Run("invalid dto rolls back directory", func(t *testing.T) {
		name := "响应失败回滚"
		key, err := service.BuildEntryNameKey(name)
		if err != nil {
			t.Fatal(err)
		}
		err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			entry, err := txStore.CreateUserDirectory(txContext, 11, name, key, nil)
			if err != nil {
				return err
			}
			entry.Revision = 2
			_, err = dto.NewCreateDirectoryData(entry, nil)
			return err
		})
		if !errors.Is(err, domain.ErrInvalidFileEntryData) {
			t.Fatal("异常创建快照未终止事务", err)
		}
		if entry, err := store.FindUserActiveFileEntryByName(ctx, 1, 11, key); entry != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("DTO 转换失败留下了新目录", err)
		}
	})
}
