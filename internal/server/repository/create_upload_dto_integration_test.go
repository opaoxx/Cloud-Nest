package repository

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestCreateUploadDataIntegration(t *testing.T) {
	// 1. 在独占库中补齐正式授权、上传任务和幂等表。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	for _, boundary := range []struct{ table, next string }{{"auth_sessions", "stored_objects"}, {"upload_tasks", "idempotency_records"}, {"idempotency_records", ""}} {
		start := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.table+" (")
		end := len(migrations.InitialSQL)
		if boundary.next != "" {
			end = strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.next+" (")
		}
		if start < 0 || end <= start {
			t.Fatal("正式响应联测表边界错误")
		}
		if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,storage_quota) VALUES(1,'create_upload_dto','创建响应用户','fixture_hash',104857600)",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','创建响应设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR')",
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
	outerFailure := errors.New("fixture stable response rollback")
	create := func(current domain.Actor, input domain.CreateUploadInput, key, mode string) (*dto.CreateUploadData, *domain.IdempotencyRecord, error) {
		requestHash, err := service.BuildCreateUploadRequestHash(input)
		if err != nil {
			return nil, nil, err
		}
		var data *dto.CreateUploadData
		var saved *domain.IdempotencyRecord
		err = store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
			// 核心服务直接复用当前事务，响应编码和保存也在同一外层回调。
			creator := service.NewCreateUploadService("local_minio", "cloudnest", func(_ context.Context, _ domain.Actor, work func(context.Context, service.CreateUploadTransaction) error) error {
				return work(txContext, txStore)
			})
			task, err := creator.Create(txContext, current, input)
			if err != nil {
				return err
			}
			if mode == "maximum task ID" {
				// 显式准备完整无符号标识夹具，再沿正式读取和转换入口核对。
				maximum := ^uint64(0)
				if err := txStore.database.WithContext(txContext).Exec("UPDATE upload_tasks SET id=? WHERE id=?", maximum, task.ID).Error; err != nil {
					return err
				}
				task, err = txStore.FindUserUploadTask(txContext, current.UserID, maximum)
				if err != nil {
					return err
				}
			}
			if mode == "invalid DTO" {
				task.Status = domain.UploadStatusVerifying
			}
			data, err = dto.NewCreateUploadData(task)
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				return err
			}
			if _, err := dto.RestoreCreateUploadData(encoded); err != nil {
				return err
			}
			saved, err = txStore.CreateUserIdempotencyRecord(txContext, domain.IdempotencyResultInput{
				Operation: domain.CreateUploadOperation, IdempotencyKey: key, RequestHash: requestHash, ResourceType: "UPLOAD_TASK", ResourceID: &task.ID, ResponseStatus: 201, ResponseBody: encoded, ExpiresAt: time.Now().Add(8 * 24 * time.Hour),
			})
			if err != nil {
				return err
			}
			if mode == "outer failure" {
				return outerFailure
			}
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
		return data, saved, nil
	}
	type creationState struct{ reserved, objects, tasks, records uint64 }
	readState := func() creationState {
		var state creationState
		if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,(SELECT COUNT(*) FROM stored_objects WHERE owner_user_id=1),(SELECT COUNT(*) FROM upload_tasks WHERE user_id=1),(SELECT COUNT(*) FROM idempotency_records WHERE user_id=1) FROM users WHERE id=1").Scan(&state.reserved, &state.objects, &state.tasks, &state.records); err != nil {
			t.Fatal(err)
		}
		return state
	}

	// 2. 单连接完成核心创建、DTO 转换与稳定 JSON 保存，外层提交后读取。
	for _, mode := range []string{"ordinary", "zero bytes", "single limit", "digest", "Unicode name"} {
		t.Run(mode, func(t *testing.T) {
			input := domain.CreateUploadInput{ParentID: 11, Name: " 学习资料.bin ", SizeBytes: 40}
			switch mode {
			case "zero bytes":
				input.SizeBytes = 0
			case "single limit":
				input.SizeBytes = domain.MaxSingleUploadSizeBytes
			case "digest":
				digest := sha256.Sum256([]byte("private-create-content"))
				input.ExpectedSHA256 = &digest
			case "Unicode name":
				input.Name = " Cafe\u0301 🌱.MD "
			}
			before := readState()
			key := "create-dto-" + strings.ReplaceAll(mode, " ", "-")
			data, saved, err := create(actor, input, key, "")
			if err != nil || data == nil || saved == nil || saved.ResourceID == nil || data.Task.ID != strconv.FormatUint(*saved.ResourceID, 10) || data.ContentPath != "/api/v1/uploads/"+data.Task.ID+"/content" || data.ExpiresAt != data.Task.ExpiresAt {
				t.Fatal("核心服务到创建 DTO 组合错误", err)
			}
			after := readState()
			if after.reserved != before.reserved+input.SizeBytes || after.objects != before.objects+1 || after.tasks != before.tasks+1 || after.records != before.records+1 {
				t.Fatal("稳定响应与三项创建事实未一起提交")
			}
			record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.CreateUploadOperation, key)
			if err != nil || record.ResponseStatus != 201 || record.ResourceType != "UPLOAD_TASK" {
				t.Fatal("上传资源范围或成功状态保存错误", err)
			}
			restored, err := dto.RestoreCreateUploadData(record.ResponseBody)
			if err != nil || !reflect.DeepEqual(data, restored) {
				t.Fatal("持久 JSON 公开字段与原始 DTO 不一致")
			}
			encoded, err := json.Marshal(dto.CreateUploadResponse{Data: *restored, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"})
			if err != nil || strings.Contains(string(encoded), "object_key") || strings.Contains(string(encoded), "private-create") || strings.Contains(string(record.ResponseBody), "request_id") {
				t.Fatal("公开 JSON 字段范围错误", err)
			}
		})
	}
	t.Run("saved snapshot remains stable", func(t *testing.T) {
		input := domain.CreateUploadInput{ParentID: 11, Name: "snapshot.bin", SizeBytes: 40}
		data, saved, err := create(actor, input, "create-dto-history", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status='VERIFYING',expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=?", *saved.ResourceID); err != nil {
			t.Fatal(err)
		}
		record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.CreateUploadOperation, "create-dto-history")
		if err != nil {
			t.Fatal(err)
		}
		restored, err := dto.RestoreCreateUploadData(record.ResponseBody)
		if err != nil || !reflect.DeepEqual(data, restored) || restored.Task.Status != "UPLOADING" {
			t.Fatal("任务后续变化改写了创建快照")
		}
		task, err := store.FindUserUploadTask(ctx, actor.UserID, *saved.ResourceID)
		if err != nil {
			t.Fatal(err)
		}
		if current, err := dto.NewCreateUploadData(task); current != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
			t.Fatal("后续阶段任务误作创建快照", err)
		}
	})
	t.Run("invalid persisted snapshots", func(t *testing.T) {
		input := domain.CreateUploadInput{ParentID: 11, Name: "invalid-snapshot.bin", SizeBytes: 40}
		data, saved, err := create(actor, input, "create-dto-invalid-snapshots", "")
		if err != nil {
			t.Fatal(err)
		}
		before := readState()
		for _, mode := range []string{"wrong path", "wrong mode", "wrong expiry", "missing null", "extra internal field"} {
			var payload map[string]json.RawMessage
			if json.Unmarshal(saved.ResponseBody, &payload) != nil {
				t.Fatal("保存快照夹具解析失败")
			}
			switch mode {
			case "wrong path":
				payload["content_path"] = json.RawMessage(`"https://private.example/content"`)
			case "wrong mode":
				payload["upload_mode"] = json.RawMessage(`"MULTIPART"`)
			case "wrong expiry":
				payload["expires_at"], err = json.Marshal(data.ExpiresAt.Add(time.Nanosecond))
			case "missing null", "extra internal field":
				var fields map[string]json.RawMessage
				if json.Unmarshal(payload["task"], &fields) != nil {
					t.Fatal("任务夹具解析失败")
				}
				if mode == "missing null" {
					delete(fields, "last_error")
				} else {
					fields["object_key"] = json.RawMessage(`"private-object"`)
				}
				payload["task"], err = json.Marshal(fields)
			}
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=? WHERE id=?", encoded, saved.ID); err != nil {
				t.Fatal(err)
			}
			record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.CreateUploadOperation, "create-dto-invalid-snapshots")
			if err != nil {
				t.Fatal(err)
			}
			if restored, err := dto.RestoreCreateUploadData(record.ResponseBody); restored != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || readState() != before {
				t.Fatal("异常持久快照未被识别或恢复改变了业务事实", err)
			}
		}
		if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=? WHERE id=?", saved.ResponseBody, saved.ID); err != nil {
			t.Fatal(err)
		}
		record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.CreateUploadOperation, "create-dto-invalid-snapshots")
		if err != nil {
			t.Fatal(err)
		}
		restored, err := dto.RestoreCreateUploadData(record.ResponseBody)
		if err != nil || !reflect.DeepEqual(data, restored) {
			t.Fatal("合法持久快照未准确恢复", err)
		}
	})
	for _, mode := range []string{"invalid DTO", "outer failure", "duplicate result"} {
		t.Run(mode, func(t *testing.T) {
			key := "create-dto-rollback-" + strings.ReplaceAll(mode, " ", "-")
			input := domain.CreateUploadInput{ParentID: 11, Name: "rollback.bin", SizeBytes: 40}
			if mode == "duplicate result" {
				if _, _, err := create(actor, input, key, ""); err != nil {
					t.Fatal(err)
				}
			}
			before := readState()
			data, saved, err := create(actor, input, key, mode)
			want := domain.ErrInvalidUploadTaskData
			if mode == "outer failure" {
				want = outerFailure
			}
			if mode == "duplicate result" {
				want = domain.ErrIdempotencyRecordAlreadyExists
			}
			if data != nil || saved != nil || !errors.Is(err, want) || readState() != before {
				t.Fatal("DTO/保存/外层失败后四项事实未回滚", err)
			}
		})
	}
	t.Run("maximum task and parent IDs", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash,storage_quota) VALUES(?,'create_dto_max','完整用户','fixture_hash',?)", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'48ee758a-0727-42de-ae7d-3bb33155e148','完整设备','WINDOWS')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"ALTER TABLE stored_objects AUTO_INCREMENT = 9223372036854775810", "ALTER TABLE upload_tasks AUTO_INCREMENT = 9223372036854775810"} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		data, _, err := create(domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}, domain.CreateUploadInput{ParentID: maximum, Name: "maximum.bin", SizeBytes: 0}, "create-dto-max", "maximum task ID")
		if err != nil || data == nil || data.Task.ID != "18446744073709551615" || data.Task.ParentID != "18446744073709551615" || data.ContentPath != "/api/v1/uploads/18446744073709551615/content" {
			t.Fatal("完整最大任务/目录标识或内容路径丢失", err)
		}
	})
}
