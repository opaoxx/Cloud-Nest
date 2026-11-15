package repository

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestEntryNameMetadataIntegration(t *testing.T) {
	// 1. 使用正式表独占库，准备根目录和文件内容对象。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','名称分类用户','fixture_hash','ACTIVE')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-name-metadata',1)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.FindUserFileEntry(ctx, 1, 11)
	if err != nil {
		t.Fatal(err)
	}
	nextID := uint64(100)
	persist := func(t *testing.T, entryType, rawName string) *dto.EntryDetailData {
		t.Helper()
		// 2. 将计算结果写入测试夹具，核对正式 CHECK 和 VARCHAR 容量。
		metadata, err := service.BuildEntryNameMetadata(entryType, rawName)
		if err != nil {
			t.Fatal(err)
		}
		nameKey, err := service.BuildEntryNameKey(metadata.Name)
		if err != nil {
			t.Fatal(err)
		}
		nextID++
		var objectID *uint64
		var mimeType *string
		var size uint64
		if entryType == "FILE" {
			referenceID := uint64(41)
			contentMIME := "application/octet-stream"
			objectID, mimeType, size = &referenceID, &contentMIME, 1
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,object_id,file_size,mime_type) VALUES(?,1,11,?,?,?,?,?,?,?,?)", nextID, metadata.Name, nameKey, entryType, metadata.Category, metadata.Extension, objectID, size, mimeType); err != nil {
			t.Fatal(err)
		}

		// 3. 使用既有事实读取和详情 DTO 核对分类及可空字段的交付形式。
		entry, err := store.FindUserFileEntry(ctx, 1, nextID)
		if err != nil {
			t.Fatal(err)
		}
		display, err := store.FindUserFileEntryDisplay(ctx, 1, nextID)
		if err != nil || !reflect.DeepEqual(display.Category, metadata.Category) || !reflect.DeepEqual(display.Extension, metadata.Extension) || !reflect.DeepEqual(display.MIMEType, mimeType) {
			t.Fatal("名称派生字段或内容 MIME 读回错误", err)
		}
		data, err := dto.NewEntryDetailData(&domain.EntryDetail{
			Path:       domain.FileEntryPath{Entries: []domain.FileEntry{*root, *entry}},
			Display:    *display,
			Preference: domain.EntryPreference{UserID: 1, EntryID: nextID},
		})
		if err != nil || data.Name != metadata.Name || !reflect.DeepEqual(data.Extension, metadata.Extension) {
			t.Fatal("名称元数据详情交付错误", err)
		}
		return data
	}
	t.Run("file categories", func(t *testing.T) {
		for _, testCase := range []struct{ name, kind, extension string }{
			{"  report.PDF  ", "document", "pdf"}, {"forest.SVG", "image", "svg"},
			{"trip.MP4", "video", "mp4"}, {"music.WAV", "audio", "wav"},
			{"archive.ZIP", "archive", "zip"}, {"payload.Custom", "other", "custom"},
		} {
			data := persist(t, "FILE", testCase.name)
			if data.Kind != testCase.kind || data.Extension == nil || *data.Extension != testCase.extension {
				t.Fatal("六类文件的字段或前端 kind 映射错误")
			}
		}
	})
	t.Run("optional suffixes and directories", func(t *testing.T) {
		for _, name := range []string{"README", ".env"} {
			data := persist(t, "FILE", name)
			if data.Kind != "other" || data.Extension != nil {
				t.Fatal("无扩展名文件字段错误")
			}
		}
		for _, name := range []string{"资料.pdf", ".settings"} {
			data := persist(t, "DIR", name)
			if data.Kind != "folder" || data.Extension != nil {
				t.Fatal("文件夹空字段或 kind 错误")
			}
		}
		data := persist(t, "FILE", ".config.JSON")
		if data.Kind != "document" || data.Extension == nil || *data.Extension != "json" {
			t.Fatal("点开头且带后缀的文件字段错误")
		}
	})
	t.Run("varchar character capacity", func(t *testing.T) {
		for _, suffix := range []string{strings.Repeat("X", 32), strings.Repeat("🌱", 32)} {
			data := persist(t, "FILE", "capacity."+suffix)
			var characters, bytes int
			if err := pool.QueryRowContext(ctx, "SELECT CHAR_LENGTH(extension),OCTET_LENGTH(extension) FROM file_entries WHERE id=?", data.ID).Scan(&characters, &bytes); err != nil || characters != 32 || bytes != len(strings.ToLower(suffix)) {
				t.Fatal("扩展名字符容量或 UTF-8 字节数错误", err)
			}
		}
		for _, suffix := range []string{strings.Repeat("x", 33), strings.Repeat("🌱", 33)} {
			data := persist(t, "FILE", "capacity."+suffix)
			var extension sql.NullString
			if err := pool.QueryRowContext(ctx, "SELECT extension FROM file_entries WHERE id=?", data.ID).Scan(&extension); err != nil || extension.Valid || data.Extension != nil || data.Kind != "other" {
				t.Fatal("超容量后缀的合法名称未准确保存为空提示", err)
			}
		}
	})
}
