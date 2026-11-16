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

func TestMoveEntryDataIntegration(t *testing.T) {
	// 1. 使用正式表独占库，准备文件、目录、内容事实、偏好和当前登录身份。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	for _, boundary := range []struct{ table, next string }{
		{"auth_sessions", "stored_objects"}, {"user_entry_preferences", "upload_tasks"}, {"idempotency_records", ""},
	} {
		start := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.table+" (")
		end := len(migrations.InitialSQL)
		if boundary.next != "" {
			end = strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.next+" (")
		}
		if start < 0 || end <= start {
			t.Fatal("正式迁移边界错误")
		}
		if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'move_dto','移动响应用户','fixture_hash','ACTIVE')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,color_key) VALUES(11,1,NULL,'',X'','DIR',NULL),(15,1,11,'资料',CAST('资料' AS BINARY),'DIR','green'),(16,1,11,'目的地',CAST('目的地' AS BINARY),'DIR',NULL)",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','响应测试设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','move-response-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,object_id,file_size,revision,mime_type,source_device_id,source_device_name,content_modified_at) VALUES(14,1,15,'report.md',CAST('report.md' AS BINARY),'FILE','DOCUMENT','md',41,2048,3,'text/markdown',13,'  原始设备 🎮  ',UTC_TIMESTAMP(6)-INTERVAL 1 DAY)",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,14,UTC_TIMESTAMP(6)-INTERVAL 1 HOUR,UTC_TIMESTAMP(6))",
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
	readDetail := func(txContext context.Context, txStore *Store, entryID uint64) (*domain.EntryDetail, error) {
		reader := service.NewEntryDetailService(func(ctx context.Context, action func(context.Context, service.EntryDetailReader) error) error {
			return action(ctx, txStore)
		})
		return reader.Get(txContext, actor, entryID)
	}
	move := func(txContext context.Context, txStore *Store, entryID, revision, targetID uint64) error {
		mover := service.NewMoveEntryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, service.MoveEntryTransaction) error) error {
			return action(ctx, txStore)
		})
		_, err := mover.Move(txContext, actor, entryID, revision, targetID)
		return err
	}
	// 测试组合移动、详情转换与稳定结果保存，统一由外层用户事务提交。
	prepareSnapshot := func(txContext context.Context, txStore *Store, entryID, revision, targetID uint64, key string) (*dto.MoveEntryData, *domain.IdempotencyRecord, error) {
		if err := move(txContext, txStore, entryID, revision, targetID); err != nil {
			return nil, nil, err
		}
		detail, err := readDetail(txContext, txStore, entryID)
		if err != nil {
			return nil, nil, err
		}
		data, err := dto.NewMoveEntryData(detail)
		if err != nil {
			return nil, nil, err
		}
		encoded, err := json.Marshal(data)
		if err != nil {
			return nil, nil, err
		}
		hash, err := service.BuildMoveEntryRequestHash(entryID, revision, targetID)
		if err != nil {
			return nil, nil, err
		}
		saved, err := txStore.CreateUserIdempotencyRecord(txContext, domain.IdempotencyResultInput{
			Operation: domain.MoveEntryOperation, IdempotencyKey: key, RequestHash: hash,
			ResourceType: "ENTRY", ResourceID: &entryID, ResponseStatus: 200, ResponseBody: encoded,
			ExpiresAt: time.Now().Add(8 * 24 * time.Hour),
		})
		return data, saved, err
	}
	snapshot := func(t *testing.T, entryID, revision, targetID uint64, key string) (*dto.MoveEntryData, *domain.IdempotencyRecord) {
		t.Helper()
		var data *dto.MoveEntryData
		var saved *domain.IdempotencyRecord
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			var err error
			data, saved, err = prepareSnapshot(txContext, txStore, entryID, revision, targetID, key)
			return err
		})
		if err != nil {
			t.Fatal("移动及响应快照事务失败", err)
		}
		if saved == nil || saved.ResourceID == nil || *saved.ResourceID != entryID || saved.Operation != domain.MoveEntryOperation || saved.ResponseStatus != 200 {
			t.Fatal("数据库快照记录错误")
		}
		decoded, restoreError := dto.RestoreMoveEntryData(saved.ResponseBody)
		if restoreError != nil || !reflect.DeepEqual(data, decoded) {
			t.Fatal("数据库快照恢复错误", restoreError)
		}
		return data, saved
	}
	var firstData *dto.MoveEntryData
	var firstRecord *domain.IdempotencyRecord

	// 2. 单连接串起移动、完整详情转换及 JSON 保存。
	t.Run("single connection file snapshot", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(2)
		firstData, firstRecord = snapshot(t, 14, 3, 16, "move-file-007")
		entry := firstData.Entry
		if entry.ID != "14" || entry.ParentID == nil || *entry.ParentID != "16" || entry.Name != "report.md" || entry.Revision != "4" || entry.Kind != "document" || entry.Extension == nil || *entry.Extension != "md" || entry.MIMEType == nil || *entry.MIMEType != "text/markdown" || entry.ContentModifiedAt == nil || entry.SourceDevice == nil || entry.SourceDevice.Name == nil || *entry.SourceDevice.Name != "  原始设备 🎮  " || !entry.Starred || entry.StarredAt == nil || entry.LastOpenedAt == nil {
			t.Fatal("文件稳定快照字段错误")
		}
		var body map[string]json.RawMessage
		if json.Unmarshal(firstRecord.ResponseBody, &body) != nil || len(body) != 1 || body["entry"] == nil {
			t.Fatal("稳定数据结构错误")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(body["entry"], &fields) != nil || len(fields) != 18 {
			t.Fatal("公开详情字段数量错误")
		}
	})
	t.Run("directory null empty and green colors", func(t *testing.T) {
		empty, green := "", "green"
		for index, color := range []*string{nil, &empty, &green} {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET color_key=? WHERE id=15", color); err != nil {
				t.Fatal(err)
			}
			targetID := uint64(16)
			if index == 1 {
				targetID = 11
			}
			data, _ := snapshot(t, 15, uint64(index+1), targetID, fmt.Sprintf("move-directory-%d", index))
			entry := data.Entry
			if entry.Kind != "folder" || entry.SizeBytes != 0 || entry.Name != "资料" || entry.Extension != nil || (entry.ColorKey == nil) != (color == nil) || (color != nil && *entry.ColorKey != *color) || entry.Starred || entry.ContentModifiedAt != nil || entry.SourceDevice != nil {
				t.Fatal("目录颜色与空值快照错误")
			}
		}
	})
	t.Run("all stored file kinds", func(t *testing.T) {
		for index, kind := range []struct{ category, public string }{{"DOCUMENT", "document"}, {"IMAGE", "image"}, {"VIDEO", "video"}, {"AUDIO", "audio"}, {"ARCHIVE", "archive"}, {"OTHER", "other"}} {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET category=? WHERE id=14", kind.category); err != nil {
				t.Fatal(err)
			}
			current, err := store.FindUserFileEntry(ctx, 1, 14)
			if err != nil {
				t.Fatal(err)
			}
			targetID := uint64(15)
			if index%2 == 1 {
				targetID = 11
			}
			data, _ := snapshot(t, 14, current.Revision, targetID, fmt.Sprintf("move-kind-%d", index))
			if data.Entry.Kind != kind.public || data.Entry.Name != "report.md" || data.Entry.Extension == nil || *data.Entry.Extension != "md" || data.Entry.MIMEType == nil || *data.Entry.MIMEType != "text/markdown" {
				t.Fatal("文件类别或原展示事实错误")
			}
		}
	})
	t.Run("saved snapshot survives later moves and preferences", func(t *testing.T) {
		if firstData == nil || firstRecord == nil {
			t.Fatal("首次快照缺失")
		}
		if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=NULL,last_opened_at=NULL WHERE user_id=1 AND entry_id=14"); err != nil {
			t.Fatal(err)
		}
		record, err := store.FindUserIdempotencyRecord(ctx, 1, domain.MoveEntryOperation, "move-file-007")
		if err != nil {
			t.Fatal(err)
		}
		decoded, restoreError := dto.RestoreMoveEntryData(record.ResponseBody)
		if restoreError != nil || !reflect.DeepEqual(firstData, decoded) || *decoded.Entry.ParentID != "16" || decoded.Entry.Revision != "4" || !decoded.Entry.Starred {
			t.Fatal("首次快照混入后续状态", restoreError)
		}
		current, err := store.FindUserFileEntry(ctx, 1, 14)
		if err != nil || current.ParentID == nil || *current.ParentID != 11 || current.Revision <= 4 {
			t.Fatal("当前状态对照错误", err)
		}
	})
	// 3. 转换失败或保存后外层失败时，移动和结果记录一起回滚。
	t.Run("conversion and outer failure rollback", func(t *testing.T) {
		failure := errors.New("fixture move snapshot rollback")
		for _, afterSave := range []bool{false, true} {
			before, err := store.FindUserFileEntry(ctx, 1, 14)
			if err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("move-rollback-%t", afterSave)
			err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				if afterSave {
					if _, _, err := prepareSnapshot(txContext, txStore, 14, before.Revision, 16, key); err != nil {
						return err
					}
					return failure
				}
				if err := move(txContext, txStore, 14, before.Revision, 16); err != nil {
					return err
				}
				detail, err := readDetail(txContext, txStore, 14)
				if err != nil {
					return err
				}
				detail.Path.Entries[len(detail.Path.Entries)-1].Name = "错误名称\xff"
				_, err = dto.NewMoveEntryData(detail)
				return err
			})
			want := domain.ErrInvalidFileEntryData
			if afterSave {
				want = failure
			}
			after, readError := store.FindUserFileEntry(ctx, 1, 14)
			if !errors.Is(err, want) || readError != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("转换或外层失败未回滚移动", err, readError)
			}
			if record, err := store.FindUserIdempotencyRecord(ctx, 1, domain.MoveEntryOperation, key); record != nil || !errors.Is(err, domain.ErrIdempotencyRecordNotFound) {
				t.Fatal("失败事务留下结果记录", err)
			}
		}
	})
	t.Run("maximum identifiers revision and same position", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,11,'高位目的地',CAST('高位目的地' AS BINARY),'DIR')", maximum-1); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,object_id,file_size,revision) VALUES(?,1,11,'高位.md',CAST('高位.md' AS BINARY),'FILE','DOCUMENT','md',41,2048,?)", maximum, maximum-1); err != nil {
			t.Fatal(err)
		}
		data, _ := snapshot(t, maximum, maximum-1, maximum-1, "move-maximum")
		if data.Entry.ID != "18446744073709551615" || data.Entry.ParentID == nil || *data.Entry.ParentID != "18446744073709551614" || data.Entry.Revision != "18446744073709551615" {
			t.Fatal("完整整数快照错误")
		}
		same, _ := snapshot(t, maximum, maximum, maximum-1, "move-maximum-same")
		if !reflect.DeepEqual(data, same) {
			t.Fatal("最大版本同位置快照错误")
		}
	})

	// 4. 合法 MySQL JSON 中的异常快照由恢复入口按统一错误拒绝。
	t.Run("invalid persisted snapshots", func(t *testing.T) {
		if firstRecord == nil {
			t.Fatal("首次快照缺失")
		}
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=? WHERE id=?", string(firstRecord.ResponseBody), firstRecord.ID); err != nil {
				t.Error("恢复原始测试快照失败", err)
			}
		}()
		for _, mode := range []string{"missing field", "source shape", "invalid id", "invalid parent", "self parent", "noncanonical revision", "unknown kind", "starred mismatch", "null size", "hidden status", "invalid time", "extra field"} {
			var payload map[string]map[string]any
			if err := json.Unmarshal(firstRecord.ResponseBody, &payload); err != nil {
				t.Fatal(err)
			}
			entry := payload["entry"]
			switch mode {
			case "missing field":
				delete(entry, "extension")
			case "source shape":
				entry["source_device"] = map[string]any{"id": "13"}
			case "invalid id":
				entry["id"] = "0"
			case "invalid parent":
				entry["parent_id"] = "016"
			case "self parent":
				entry["parent_id"] = "14"
			case "noncanonical revision":
				entry["revision"] = "04"
			case "unknown kind":
				entry["kind"] = "UNKNOWN"
			case "starred mismatch":
				entry["starred"] = false
			case "null size":
				entry["size_bytes"] = nil
			case "hidden status":
				entry["status"] = "TRASHED"
			case "invalid time":
				entry["updated_at"] = "0999-12-31T23:59:59Z"
			case "extra field":
				entry["request_id"] = "private"
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=? WHERE id=?", string(encoded), firstRecord.ID); err != nil {
				t.Fatal(err)
			}
			record, err := store.FindUserIdempotencyRecord(ctx, 1, domain.MoveEntryOperation, "move-file-007")
			if err != nil {
				t.Fatal("通用 JSON 记录读取失败", err)
			}
			if data, err := dto.RestoreMoveEntryData(record.ResponseBody); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || err.Error() != "幂等记录数据异常" {
				t.Fatal("异常持久快照未拒绝", mode, err)
			}
		}
	})
}
