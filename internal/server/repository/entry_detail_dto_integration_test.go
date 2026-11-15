package repository

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestEntryDetailDTOIntegration(t *testing.T) {
	// 1. 复用正式表独占库和详情服务，增加正式偏好表及展示夹具。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE user_entry_preferences (")
	if start < 0 {
		t.Fatal("未找到正式偏好表")
	}
	end := strings.Index(migrations.InitialSQL[start:], "CREATE TABLE upload_tasks (")
	if end < 0 {
		t.Fatal("未找到偏好表结束边界")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:start+end]); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','详情响应用户','fixture_hash')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(19,1,'28ee758a-0727-42de-ae7d-3bb33155e148','设备当前名称','WEB')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-detail-dto',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,color_key) VALUES(11,1,NULL,'',X'','DIR',NULL),(12,1,11,'课程资料',CAST('课程资料' AS BINARY),'DIR','sand')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,mime_type,object_id,file_size,revision,source_device_id,source_device_name,content_modified_at) VALUES(23,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT','md','text/markdown',41,2048,3,19,'  发布时设备 🎮  ','2026-11-15 01:30:00.123456')",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,23,'2026-11-15 02:30:00.123456','2026-11-15 03:30:00.123456')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立详情 DTO 夹具失败", err)
		}
	}
	createdAt := time.Date(2026, 11, 15, 1, 30, 0, 123456000, time.UTC)
	modifiedAt := createdAt.Add(time.Hour)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", createdAt, modifiedAt, modifiedAt); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	details := service.NewEntryDetailService(func(ctx context.Context, action func(context.Context, service.EntryDetailReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
			return action(snapshotContext, snapshotStore)
		})
	})
	actor := domain.Actor{UserID: 1, SessionID: 17, DeviceID: 19}
	read := func(t *testing.T, identity domain.Actor, entryID uint64) *dto.EntryDetailData {
		t.Helper()
		result, err := details.Get(ctx, identity, entryID)
		if err != nil {
			t.Fatal(err)
		}
		data, err := dto.NewEntryDetailData(result)
		if err != nil || data == nil {
			t.Fatal("真实详情转换失败", err)
		}
		return data
	}

	// 2. 验证公开字段、根目录 null、颜色与默认偏好的实际转换。
	t.Run("file directory root and JSON", func(t *testing.T) {
		data := read(t, actor, 23)
		if data.ID != "23" || data.ParentID == nil || *data.ParentID != "12" || data.Kind != "document" || data.SizeBytes != 2048 || data.Revision != "3" ||
			data.SourceDevice == nil || *data.SourceDevice.ID != "19" || *data.SourceDevice.Name != "  发布时设备 🎮  " || !data.Starred ||
			!data.CreatedAt.Equal(createdAt) || !data.ModifiedAt.Equal(modifiedAt) || !data.ContentModifiedAt.Equal(createdAt) || !data.StarredAt.Equal(modifiedAt) || !data.LastOpenedAt.Equal(modifiedAt.Add(time.Hour)) {
			t.Fatal("真实文件详情、来源名称或偏好转换错误")
		}
		response := dto.EntryDetailResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}
		encoded, err := json.Marshal(response)
		var outer map[string]json.RawMessage
		if err != nil || json.Unmarshal(encoded, &outer) != nil || len(outer) != 2 {
			t.Fatal("真实详情 JSON 外层错误")
		}
		var payload map[string]any
		if json.Unmarshal(outer["data"], &payload) != nil || len(payload) != 18 || payload["created_at"] != "2026-11-15T01:30:00.123456Z" || payload["color_key"] != nil {
			t.Fatal("真实详情 JSON 字段或时间错误")
		}
		directory := read(t, actor, 12)
		root := read(t, actor, 11)
		if directory.Kind != "folder" || directory.ColorKey == nil || *directory.ColorKey != "sand" || directory.Starred || directory.SourceDevice != nil ||
			root.Kind != "folder" || root.ParentID != nil || root.Name != "" || root.SizeBytes != 0 || root.StarredAt != nil || root.LastOpenedAt != nil {
			t.Fatal("真实目录、颜色、根目录或默认偏好错误")
		}
	})
	t.Run("six saved file categories", func(t *testing.T) {
		for _, category := range []string{"DOCUMENT", "IMAGE", "VIDEO", "AUDIO", "ARCHIVE", "OTHER"} {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET category=? WHERE id=23", category); err != nil {
				t.Fatal(err)
			}
			if data := read(t, actor, 23); data.Kind != strings.ToLower(category) {
				t.Fatal("真实文件类别映射错误")
			}
		}
	})
	t.Run("partial historical source and empty text", func(t *testing.T) {
		for _, mode := range []string{"name only", "id only", "all missing", "empty text"} {
			t.Run(mode, func(t *testing.T) {
				var sourceID, sourceName any = uint64(19), "发布时名称"
				extension := "md"
				switch mode {
				case "name only":
					sourceID = nil
				case "id only":
					sourceName = nil
				case "all missing":
					sourceID, sourceName = nil, nil
				case "empty text":
					sourceName, extension = "", ""
				}
				if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET source_device_id=?,source_device_name=?,extension=? WHERE id=23", sourceID, sourceName, extension); err != nil {
					t.Fatal(err)
				}
				data := read(t, actor, 23)
				if data.Extension == nil || *data.Extension != extension {
					t.Fatal("真实空扩展名未保留")
				}
				if mode == "all missing" {
					if data.SourceDevice != nil {
						t.Fatal("来源全部缺失时应为 null")
					}
					return
				}
				if data.SourceDevice == nil || (data.SourceDevice.ID == nil) != (sourceID == nil) || (data.SourceDevice.Name == nil) != (sourceName == nil) {
					t.Fatal("真实历史来源空值转换错误")
				}
				if mode == "empty text" && *data.SourceDevice.Name != "" {
					t.Fatal("来源空名称未保留")
				}
			})
		}
	})

	// 3. 最大标识夹具最后写入，验证完整字符串 ID 与公开字节边界。
	t.Run("full unsigned identifiers and numeric overflow", func(t *testing.T) {
		maximum := ^uint64(0)
		const safeSize uint64 = 1<<53 - 1
		minimumDate := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
		maximumDate := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','完整整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'edbf7e35-36ea-45a5-bdb9-58e76b46fd5e','完整整数设备','ANDROID')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','fixture-detail-dto-max',?)", maximum, maximum, safeSize); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum-1, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision,source_device_id,content_modified_at,created_at,modified_at,updated_at) VALUES(?,?,?,'最大文件.bin',CAST('最大文件.bin' AS BINARY),'FILE','OTHER',?,?,?,?,?,?,?,?)", maximum, maximum, maximum-1, maximum, safeSize, maximum, maximum, minimumDate, minimumDate, maximumDate, maximumDate); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(?,?,?,?)", maximum, maximum, minimumDate, maximumDate); err != nil {
			t.Fatal(err)
		}
		identity := domain.Actor{UserID: maximum, SessionID: maximum, DeviceID: maximum}
		data := read(t, identity, maximum)
		if data.ID != strconv.FormatUint(maximum, 10) || data.Revision != data.ID || data.ParentID == nil || *data.ParentID != strconv.FormatUint(maximum-1, 10) ||
			data.SizeBytes != safeSize || data.SourceDevice == nil || *data.SourceDevice.ID != data.ID || data.SourceDevice.Name != nil ||
			!data.CreatedAt.Equal(minimumDate) || !data.ModifiedAt.Equal(maximumDate) || !data.ContentModifiedAt.Equal(minimumDate) || !data.StarredAt.Equal(minimumDate) || !data.LastOpenedAt.Equal(maximumDate) {
			t.Fatal("完整标识、安全字节数或数据库日期边界转换错误")
		}
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET file_size=? WHERE id=?", safeSize+1, maximum); err != nil {
			t.Fatal(err)
		}
		unsafe, err := details.Get(ctx, identity, maximum)
		if err != nil {
			t.Fatal(err)
		}
		if converted, err := dto.NewEntryDetailData(unsafe); converted != nil || err == nil {
			t.Fatal("超出 JSON 安全范围的字节值仍完成交付转换")
		}
	})
}
