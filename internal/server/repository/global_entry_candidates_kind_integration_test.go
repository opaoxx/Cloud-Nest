package repository

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListUserGlobalEntryCandidatesKindIntegration(t *testing.T) {
	// 1. 使用正式表准备目录、六类文件和其他账号及隐藏状态。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','分类候选用户','fixture_hash'),(3,'fixture_other','其他用户','fixture_hash'),(5,'fixture_empty','空用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-kind-object',2048),(43,3,'MINIO','fixture-bucket','fixture-other-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(55,5,NULL,'',X'','DIR'),(12,1,11,'课程目录',CAST('课程目录' AS BINARY),'DIR'),(14,1,12,'子目录',CAST('子目录' AS BINARY),'DIR'),(34,3,33,'其他用户目录',CAST('其他用户目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(16,1,11,'回收目录',CAST('回收目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(15,1,16,'回收祖先下活动目录',CAST('回收祖先下活动目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(101,1,12,'名字像图片.png',CAST('名字像图片.png' AS BINARY),'FILE','DOCUMENT',41,2048,1),(102,1,14,'课程.md',CAST('课程.md' AS BINARY),'FILE','DOCUMENT',41,2048,2),(103,1,11,'根下文档.md',CAST('根下文档.md' AS BINARY),'FILE','DOCUMENT',41,2048,3),(104,1,16,'回收祖先下活动文档.md',CAST('回收祖先下活动文档.md' AS BINARY),'FILE','DOCUMENT',41,2048,1),(105,1,11,'图片.jpg',CAST('图片.jpg' AS BINARY),'FILE','IMAGE',41,2048,1),(106,1,12,'视频.mp4',CAST('视频.mp4' AS BINARY),'FILE','VIDEO',41,2048,1),(107,1,14,'音乐.mp3',CAST('音乐.mp3' AS BINARY),'FILE','AUDIO',41,2048,1),(108,1,11,'压缩包.zip',CAST('压缩包.zip' AS BINARY),'FILE','ARCHIVE',41,2048,1),(109,1,12,'其他文件.docx',CAST('其他文件.docx' AS BINARY),'FILE','OTHER',41,2048,1),(201,3,33,'其他账号文档.md',CAST('其他账号文档.md' AS BINARY),'FILE','DOCUMENT',43,2048,1)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,status,deleted_at) VALUES(111,1,12,'回收文档.md',CAST('回收文档.md' AS BINARY),'FILE','DOCUMENT',41,2048,'TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,status,purge_started_at) VALUES(112,1,12,'清理中图片.jpg',CAST('清理中图片.jpg' AS BINARY),'FILE','IMAGE',41,2048,'PURGING',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,file_size,status,purged_at) VALUES(113,1,12,'已清理图片.jpg',CAST('已清理图片.jpg' AS BINARY),'FILE','IMAGE',2048,'PURGED',UTC_TIMESTAMP(6))",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立分类候选夹具失败", err)
		}
	}
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", stamp.Add(-time.Hour), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=? WHERE id IN (102,103)", stamp.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=? WHERE id=101", stamp.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	checkPage := func(t *testing.T, page *domain.FileEntryPage, ids []uint64, more bool) {
		t.Helper()
		if page == nil || page.Entries == nil || len(page.Entries) != len(ids) || page.HasMore != more {
			t.Fatal("分类候选页长度、空切片或 HasMore 错误")
		}
		for index, entry := range page.Entries {
			if entry.ID != ids[index] || entry.Status != "ACTIVE" || entry.ParentID == nil {
				t.Fatal("分类候选顺序或范围错误")
			}
		}
	}

	// 2. 验证各类筛选及完整数据，确认筛选发生在分页前。
	for _, fixture := range []struct {
		kind string
		ids  []uint64
	}{
		{kind: "", ids: []uint64{103, 102, 101, 109, 108, 107, 106, 105, 104, 15, 14, 12}},
		{kind: "folder", ids: []uint64{15, 14, 12}},
		{kind: "document", ids: []uint64{103, 102, 101, 104}},
		{kind: "image", ids: []uint64{105}},
		{kind: "video", ids: []uint64{106}},
		{kind: "audio", ids: []uint64{107}},
		{kind: "archive", ids: []uint64{108}},
		{kind: "other", ids: []uint64{109}},
	} {
		t.Run("kind="+fixture.kind, func(t *testing.T) {
			query := globalEntryCandidateQuery(100)
			query.Kind = fixture.kind
			page, err := store.ListUserGlobalEntryCandidates(ctx, 1, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkPage(t, page, fixture.ids, false)
			for _, entry := range page.Entries {
				stored, err := store.FindUserFileEntry(ctx, 1, entry.ID)
				if err != nil || stored == nil || !reflect.DeepEqual(entry, *stored) {
					t.Fatal("分类读取影响了基础字段", err)
				}
				display, err := store.FindUserFileEntryDisplay(ctx, 1, entry.ID)
				if err != nil {
					t.Fatal(err)
				}
				if fixture.kind == "folder" && (entry.EntryType != "DIR" || display.Category != nil) {
					t.Fatal("文件夹筛选与正式表事实不符")
				}
				if fixture.kind != "" && fixture.kind != "folder" && (entry.EntryType != "FILE" || display.Category == nil || *display.Category != strings.ToUpper(fixture.kind)) {
					t.Fatal("文件类别筛选与正式表事实不符")
				}
			}
		})
	}
	t.Run("filtered tuple pages and has more", func(t *testing.T) {
		query := globalEntryCandidateQuery(1)
		query.Kind = "document"
		var before *domain.FileEntryPosition
		for index, id := range []uint64{103, 102, 101, 104} {
			page, err := store.ListUserGlobalEntryCandidates(ctx, 1, query, before)
			if err != nil {
				t.Fatal(err)
			}
			checkPage(t, page, []uint64{id}, index < 3)
			entry := page.Entries[0]
			before = &domain.FileEntryPosition{ModifiedAt: entry.ModifiedAt, ID: entry.ID}
		}
		empty, err := store.ListUserGlobalEntryCandidates(ctx, 1, query, before)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, empty, nil, false)
		query.Kind = "other"
		one, err := store.ListUserGlobalEntryCandidates(ctx, 1, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, one, []uint64{109}, false)
	})

	// 3. 验证用户范围、空结果和筛选条件的快照一致性。
	t.Run("ownership and empty results", func(t *testing.T) {
		for _, kind := range []string{"", "folder", "document", "image", "video", "audio", "archive", "other"} {
			query := globalEntryCandidateQuery(50)
			query.Kind = kind
			for _, userID := range []uint64{5, 999} {
				empty, err := store.ListUserGlobalEntryCandidates(ctx, userID, query, nil)
				if err != nil {
					t.Fatal(err)
				}
				checkPage(t, empty, nil, false)
			}
		}
		query := globalEntryCandidateQuery(50)
		query.Kind = "document"
		foreign, err := store.ListUserGlobalEntryCandidates(ctx, 3, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, foreign, []uint64{201}, false)
		query.Kind = "folder"
		foreign, err = store.ListUserGlobalEntryCandidates(ctx, 3, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, foreign, []uint64{34}, false)
	})
	t.Run("category change respects snapshot", func(t *testing.T) {
		query := globalEntryCandidateQuery(1)
		query.Kind = "document"
		if err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			first, err := txStore.ListUserGlobalEntryCandidates(txContext, 1, query, nil)
			if err != nil {
				return err
			}
			checkPage(t, first, []uint64{103}, true)
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET category='IMAGE',modified_at=?,updated_at=? WHERE id=103", stamp.Add(10*time.Second), stamp.Add(10*time.Second)); err != nil {
				return err
			}
			again, err := txStore.ListUserGlobalEntryCandidates(txContext, 1, query, nil)
			if err != nil || !reflect.DeepEqual(first, again) {
				t.Fatal("分类快照混入后续修改", err)
			}
			last := first.Entries[0]
			next, err := txStore.ListUserGlobalEntryCandidates(txContext, 1, query, &domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID})
			if err != nil {
				return err
			}
			checkPage(t, next, []uint64{102}, true)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		query.PageSize = 100
		current, err := store.ListUserGlobalEntryCandidates(ctx, 1, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, current, []uint64{102, 101, 104}, false)
		query.Kind = "image"
		current, err = store.ListUserGlobalEntryCandidates(ctx, 1, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, current, []uint64{103, 105}, false)
	})
}
