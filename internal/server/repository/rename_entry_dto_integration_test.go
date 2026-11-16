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

func TestRenameEntryDataIntegration(t *testing.T) {
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
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'rename_dto','重命名响应用户','fixture_hash','ACTIVE')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,color_key) VALUES(11,1,NULL,'',X'','DIR',NULL),(15,1,11,'资料',CAST('资料' AS BINARY),'DIR','green')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','响应测试设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','rename-response-object',2048)",
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
	// 此处直接使用当前事务，最外层用户事务统一确认所有写入。
	rename := func(txContext context.Context, txStore *Store, entryID, revision uint64, name string) error {
		renamer := service.NewRenameEntryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, service.RenameEntryTransaction) error) error {
			return action(ctx, txStore)
		})
		_, err := renamer.Rename(txContext, actor, entryID, revision, name)
		return err
	}
	snapshot := func(t *testing.T, entryID, revision uint64, name, key string) (*dto.RenameEntryData, *domain.IdempotencyRecord) {
		t.Helper()
		var data *dto.RenameEntryData
		var saved *domain.IdempotencyRecord
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if err := rename(txContext, txStore, entryID, revision, name); err != nil {
				return err
			}
			detail, err := readDetail(txContext, txStore, entryID)
			if err != nil {
				return err
			}
			data, err = dto.NewRenameEntryData(detail)
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				return err
			}
			hash, err := service.BuildRenameEntryRequestHash(entryID, revision, name)
			if err != nil {
				return err
			}
			saved, err = txStore.CreateUserIdempotencyRecord(txContext, domain.IdempotencyResultInput{
				Operation: domain.RenameEntryOperation, IdempotencyKey: key, RequestHash: hash,
				ResourceType: "ENTRY", ResourceID: &entryID, ResponseStatus: 200, ResponseBody: encoded,
				ExpiresAt: time.Now().Add(8 * 24 * time.Hour),
			})
			return err
		})
		if err != nil {
			t.Fatal("重命名与响应快照事务失败", err)
		}
		if saved == nil || saved.ResourceID == nil || *saved.ResourceID != entryID || saved.Operation != domain.RenameEntryOperation || saved.ResponseStatus != 200 {
			t.Fatal("数据库快照记录错误")
		}
		decoded, restoreError := dto.RestoreRenameEntryData(saved.ResponseBody)
		if restoreError != nil || !reflect.DeepEqual(data, decoded) {
			t.Fatal("数据库快照读回错误")
		}
		return data, saved
	}
	var firstData *dto.RenameEntryData
	var firstRecord *domain.IdempotencyRecord

	// 2. 文件写入、完整详情转换与稳定结果保存复用单条数据库连接。
	t.Run("single connection file snapshot", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(2)
		firstData, firstRecord = snapshot(t, 14, 3, "  Cafe\u0301.PDF  ", "rename-file-007")
		entry := firstData.Entry
		if entry.ID != "14" || entry.ParentID == nil || *entry.ParentID != "15" || entry.Name != "Cafe\u0301.PDF" || entry.Revision != "4" || entry.Kind != "document" || entry.Extension == nil || *entry.Extension != "pdf" || entry.MIMEType == nil || *entry.MIMEType != "text/markdown" || entry.ContentModifiedAt == nil || entry.SourceDevice == nil || entry.SourceDevice.Name == nil || *entry.SourceDevice.Name != "  原始设备 🎮  " || !entry.Starred || entry.StarredAt == nil || entry.LastOpenedAt == nil {
			t.Fatal("文件稳定快照字段错误")
		}
		var body map[string]json.RawMessage
		if json.Unmarshal(firstRecord.ResponseBody, &body) != nil || len(body) != 1 || body["entry"] == nil {
			t.Fatal("稳定数据结构错误")
		}
	})
	t.Run("directory null empty and green colors", func(t *testing.T) {
		empty, green := "", "green"
		for index, color := range []*string{nil, &empty, &green} {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET color_key=? WHERE id=15", color); err != nil {
				t.Fatal(err)
			}
			data, _ := snapshot(t, 15, uint64(index+1), fmt.Sprintf("目录%d.PDF", index), fmt.Sprintf("rename-directory-%d", index))
			entry := data.Entry
			if entry.Kind != "folder" || entry.SizeBytes != 0 || entry.Extension != nil || (entry.ColorKey == nil) != (color == nil) || (color != nil && *entry.ColorKey != *color) || entry.Starred {
				t.Fatal("目录颜色与空值快照错误")
			}
		}
	})
	t.Run("all file kinds", func(t *testing.T) {
		for index, testCase := range []struct{ suffix, kind string }{{"pdf", "document"}, {"jpg", "image"}, {"mp4", "video"}, {"mp3", "audio"}, {"zip", "archive"}, {"data", "other"}} {
			current, err := store.FindUserFileEntry(ctx, 1, 14)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := snapshot(t, 14, current.Revision, "类型."+testCase.suffix, fmt.Sprintf("rename-kind-%d", index))
			if data.Entry.Kind != testCase.kind || data.Entry.Extension == nil || *data.Entry.Extension != testCase.suffix {
				t.Fatal("文件类别快照错误")
			}
		}
	})
	t.Run("saved snapshot survives later changes", func(t *testing.T) {
		if firstData == nil || firstRecord == nil {
			t.Fatal("首次快照缺失")
		}
		record, err := store.FindUserIdempotencyRecord(ctx, 1, domain.RenameEntryOperation, "rename-file-007")
		if err != nil {
			t.Fatal(err)
		}
		decoded, restoreError := dto.RestoreRenameEntryData(record.ResponseBody)
		if restoreError != nil || !reflect.DeepEqual(firstData, decoded) || decoded.Entry.Name != "Cafe\u0301.PDF" || decoded.Entry.Revision != "4" {
			t.Fatal("首次快照混入后续状态")
		}
		current, err := store.FindUserFileEntry(ctx, 1, 14)
		if err != nil || current.Name == decoded.Entry.Name || fmt.Sprint(current.Revision) == decoded.Entry.Revision {
			t.Fatal("当前状态对照错误", err)
		}
	})

	// 3. 转换失败或保存后外层失败，名称更新与幂等快照均回滚。
	t.Run("conversion failure rolls back rename", func(t *testing.T) {
		before, err := store.FindUserFileEntry(ctx, 1, 14)
		if err != nil {
			t.Fatal(err)
		}
		err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if err := rename(txContext, txStore, 14, before.Revision, "转换失败.ZIP"); err != nil {
				return err
			}
			detail, err := readDetail(txContext, txStore, 14)
			if err != nil {
				return err
			}
			detail.Path.Entries[len(detail.Path.Entries)-1].ModifiedAt = time.Time{}
			data, err := dto.NewRenameEntryData(detail)
			if data != nil {
				return errors.New("异常详情返回了快照")
			}
			return err
		})
		after, readError := store.FindUserFileEntry(ctx, 1, 14)
		if !errors.Is(err, domain.ErrInvalidFileEntryData) || readError != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("转换失败未回滚改名", err, readError)
		}
	})
	t.Run("saved result and rename rollback together", func(t *testing.T) {
		before, err := store.FindUserFileEntry(ctx, 1, 14)
		if err != nil {
			t.Fatal(err)
		}
		failure := errors.New("fixture after snapshot save")
		err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if err := rename(txContext, txStore, 14, before.Revision, "保存后回滚.PDF"); err != nil {
				return err
			}
			detail, err := readDetail(txContext, txStore, 14)
			if err != nil {
				return err
			}
			data, err := dto.NewRenameEntryData(detail)
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				return err
			}
			hash, err := service.BuildRenameEntryRequestHash(14, before.Revision, "保存后回滚.PDF")
			if err != nil {
				return err
			}
			entryID := uint64(14)
			if _, err := txStore.CreateUserIdempotencyRecord(txContext, domain.IdempotencyResultInput{
				Operation: domain.RenameEntryOperation, IdempotencyKey: "rename-rollback", RequestHash: hash,
				ResourceType: "ENTRY", ResourceID: &entryID, ResponseStatus: 200, ResponseBody: encoded,
				ExpiresAt: time.Now().Add(8 * 24 * time.Hour),
			}); err != nil {
				return err
			}
			return failure
		})
		after, readError := store.FindUserFileEntry(ctx, 1, 14)
		if !errors.Is(err, failure) || readError != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("保存后失败未回滚改名", err, readError)
		}
		if record, err := store.FindUserIdempotencyRecord(ctx, 1, domain.RenameEntryOperation, "rename-rollback"); record != nil || !errors.Is(err, domain.ErrIdempotencyRecordNotFound) {
			t.Fatal("保存后失败仍保留幂等记录", err)
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
		for _, mode := range []string{"missing field", "source shape", "invalid id", "unknown kind", "starred mismatch", "null size", "hidden status", "extra field"} {
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
			case "unknown kind":
				entry["kind"] = "UNKNOWN"
			case "starred mismatch":
				entry["starred"] = false
			case "null size":
				entry["size_bytes"] = nil
			case "hidden status":
				entry["status"] = "TRASHED"
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
			record, err := store.FindUserIdempotencyRecord(ctx, 1, domain.RenameEntryOperation, "rename-file-007")
			if err != nil {
				t.Fatal("通用 JSON 记录读取失败", err)
			}
			if data, err := dto.RestoreRenameEntryData(record.ResponseBody); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || err.Error() != "幂等记录数据异常" {
				t.Fatal("异常持久快照未拒绝", mode, err)
			}
		}
	})
}
