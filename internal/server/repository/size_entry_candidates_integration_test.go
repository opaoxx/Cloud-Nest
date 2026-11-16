package repository

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestListUserSizeEntryCandidatesIntegration(t *testing.T) {
	// 1. 在独占库使用正式表准备大小、同值、零字节及用户/收藏交集。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE user_entry_preferences (")
	if start < 0 {
		t.Fatal("未找到正式偏好表")
	}
	end := strings.Index(migrations.InitialSQL[start:], "CREATE TABLE upload_tasks (")
	if end < 0 {
		t.Fatal("未找到正式偏好表结束边界")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:start+end]); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_size_one','大小用户','fixture_hash'),(3,'fixture_size_other','其他大小用户','fixture_hash'),(5,'fixture_size_batch','大小批次用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-size',8192),(43,3,'MINIO','fixture-bucket','fixture-size-other',16384)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(55,5,NULL,'',X'','DIR'),(12,1,11,'Parent-one',CAST('parent-one' AS BINARY),'DIR'),(14,1,11,'Parent-two',CAST('parent-two' AS BINARY),'DIR'),(107,1,12,'Docs',CAST('docs' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(16,1,11,'Trashed-parent',CAST('trashed-parent' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(101,1,12,'ReadMe.MD',CAST('readme.md' AS BINARY),'FILE','DOCUMENT',41,4096),(103,1,14,'readme.md',CAST('readme.md' AS BINARY),'FILE','DOCUMENT',41,4096),(105,1,12,'a%_.png',CAST('a%_.png' AS BINARY),'FILE','IMAGE',41,1024),(109,1,12,'empty.bin',CAST('empty.bin' AS BINARY),'FILE','OTHER',41,0),(111,1,16,'hidden.md',CAST('hidden.md' AS BINARY),'FILE','DOCUMENT',41,8192),(121,3,33,'ReadMe.MD',CAST('readme.md' AS BINARY),'FILE','DOCUMENT',43,16384)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(113,1,12,'trash',CAST('trash' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at) VALUES(115,1,12,'purging',CAST('purging' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purged_at) VALUES(117,1,12,'purged',CAST('purged' AS BINARY),'DIR','PURGED',UTC_TIMESTAMP(6))",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,11,UTC_TIMESTAMP(6),NULL),(1,101,UTC_TIMESTAMP(6),NULL),(1,103,UTC_TIMESTAMP(6),NULL),(1,107,UTC_TIMESTAMP(6),NULL),(1,111,UTC_TIMESTAMP(6),NULL),(1,105,NULL,UTC_TIMESTAMP(6)),(3,121,UTC_TIMESTAMP(6),NULL)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, page *domain.FileEntryPage, ids []uint64, more bool) {
		t.Helper()
		if page == nil || page.Entries == nil || len(page.Entries) != len(ids) || page.HasMore != more {
			t.Fatal("大小候选页形状错误")
		}
		for index, entry := range page.Entries {
			stored, err := store.FindUserFileEntry(ctx, entry.UserID, entry.ID)
			if err != nil || stored == nil || entry.ID != ids[index] || !reflect.DeepEqual(entry, *stored) {
				t.Fatal("大小候选十一字段与数据库事实不符", err)
			}
		}
	}
	t.Run("visible size lists", func(t *testing.T) { verifySizeEntryList(t, ctx, pool, store) })
	for _, scope := range []string{"all", "starred"} {
		t.Run("pagination "+scope, func(t *testing.T) {
			query := sizeEntryCandidateQuery(2)
			query.Scope = scope
			expected := []uint64{111, 103, 101, 105, 109, 107, 14, 12}
			if scope == "starred" {
				expected = []uint64{111, 103, 101, 107}
			}
			var before *domain.SizeEntryPosition
			for offset := 0; offset < len(expected); offset += 2 {
				page, err := store.ListUserSizeEntryCandidates(ctx, 1, query, before)
				if err != nil {
					t.Fatal(err)
				}
				check(t, page, expected[offset:min(offset+2, len(expected))], offset+2 < len(expected))
				last := page.Entries[len(page.Entries)-1]
				before = &domain.SizeEntryPosition{FileSize: last.FileSize, ID: last.ID}
			}
			page, err := store.ListUserSizeEntryCandidates(ctx, 1, query, before)
			if err != nil {
				t.Fatal(err)
			}
			check(t, page, nil, false)
		})
	}
	t.Run("filters and isolation", func(t *testing.T) {
		for _, fixture := range []struct {
			scope, kind, search string
			userID              uint64
			ids                 []uint64
		}{
			{"all", "", "", 3, []uint64{121}}, {"starred", "", "", 3, []uint64{121}}, {"all", "", "", 999, nil}, {"all", "", "", 5, nil},
			{"all", "folder", "", 1, []uint64{107, 14, 12}}, {"all", "document", "readme", 1, []uint64{103, 101}},
			{"starred", "document", "", 1, []uint64{111, 103, 101}}, {"starred", "image", "", 1, nil},
			{"all", "image", "%_", 1, []uint64{105}}, {"all", "other", "", 1, []uint64{109}},
			{"all", "video", "", 1, nil}, {"all", "audio", "", 1, nil}, {"all", "archive", "", 1, nil},
			{"all", "", "' OR TRUE --", 1, nil},
		} {
			query := sizeEntryCandidateQuery(100)
			query.Scope, query.Kind, query.Search = fixture.scope, fixture.kind, fixture.search
			page, err := store.ListUserSizeEntryCandidates(ctx, fixture.userID, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			check(t, page, fixture.ids, false)
		}
	})
	t.Run("zero bound and independent result", func(t *testing.T) {
		page, err := store.ListUserSizeEntryCandidates(ctx, 1, sizeEntryCandidateQuery(100), &domain.SizeEntryPosition{ID: ^uint64(0)})
		if err != nil {
			t.Fatal(err)
		}
		check(t, page, []uint64{109, 107, 14, 12}, false)
		position := domain.SizeEntryPosition{FileSize: 4096, ID: 104}
		first, err := store.ListUserSizeEntryCandidates(ctx, 1, sizeEntryCandidateQuery(1), &position)
		if err != nil {
			t.Fatal(err)
		}
		check(t, first, []uint64{103}, true)
		first.Entries[0].Name, first.Entries[0].FileSize, *first.Entries[0].ParentID = "本地修改", 0, 999
		again, err := store.ListUserSizeEntryCandidates(ctx, 1, sizeEntryCandidateQuery(1), &position)
		if err != nil || position != (domain.SizeEntryPosition{FileSize: 4096, ID: 104}) {
			t.Fatal("大小位置输入被修改", err)
		}
		check(t, again, []uint64{103}, true)
	})
	t.Run("size and membership snapshot", func(t *testing.T) {
		defer func() {
			for _, statement := range []string{
				"DELETE FROM user_entry_preferences WHERE user_id=1 AND entry_id=112", "DELETE FROM file_entries WHERE id=112",
				"UPDATE file_entries SET file_size=4096 WHERE id=103", "UPDATE user_entry_preferences SET starred_at=UTC_TIMESTAMP(6) WHERE user_id=1 AND entry_id=103",
			} {
				if _, err := pool.ExecContext(ctx, statement); err != nil {
					t.Error("恢复大小快照夹具失败", err)
				}
			}
		}()
		err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, reader *Store) error {
			allQuery, starQuery := sizeEntryCandidateQuery(2), sizeEntryCandidateQuery(2)
			starQuery.Scope = "starred"
			original, err := reader.ListUserSizeEntryCandidates(snapshotContext, 1, allQuery, nil)
			if err != nil {
				return err
			}
			starred, err := reader.ListUserSizeEntryCandidates(snapshotContext, 1, starQuery, nil)
			if err != nil {
				return err
			}
			writer, err := pool.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer writer.Rollback()
			for _, statement := range []string{
				"UPDATE file_entries SET file_size=16384 WHERE id=103", "UPDATE user_entry_preferences SET starred_at=NULL WHERE user_id=1 AND entry_id=103",
				"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(112,1,12,'inserted.md',CAST('inserted.md' AS BINARY),'FILE','DOCUMENT',41,32768)",
				"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) VALUES(1,112,UTC_TIMESTAMP(6))",
			} {
				if _, err := writer.ExecContext(ctx, statement); err != nil {
					return err
				}
			}
			if err := writer.Commit(); err != nil {
				return err
			}
			current, err := reader.ListUserSizeEntryCandidates(snapshotContext, 1, allQuery, nil)
			if err != nil || !reflect.DeepEqual(current, original) {
				return fmt.Errorf("大小快照混入新大小或插入项: %w", err)
			}
			current, err = reader.ListUserSizeEntryCandidates(snapshotContext, 1, starQuery, nil)
			if err != nil || !reflect.DeepEqual(current, starred) {
				return fmt.Errorf("大小快照混入收藏变更: %w", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		query := sizeEntryCandidateQuery(2)
		page, err := store.ListUserSizeEntryCandidates(ctx, 1, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, page, []uint64{112, 103}, true)
		query.Scope = "starred"
		page, err = store.ListUserSizeEntryCandidates(ctx, 1, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, page, []uint64{112, 111}, true)
	})
	t.Run("uncommitted writer", func(t *testing.T) {
		writer, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		if _, err := writer.ExecContext(ctx, "UPDATE file_entries SET file_size=32768 WHERE id=103"); err != nil {
			t.Fatal(err)
		}
		current, release := context.WithTimeout(ctx, 500*time.Millisecond)
		defer release()
		page, err := store.ListUserSizeEntryCandidates(current, 1, sizeEntryCandidateQuery(2), nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, page, []uint64{111, 103}, true)
	})
	t.Run("maximum page", func(t *testing.T) {
		for index := 0; index < 101; index++ {
			name := fmt.Sprintf("batch-%03d", index)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,5,55,?,?,'DIR')", 1000+index, name, []byte(name)); err != nil {
				t.Fatal(err)
			}
		}
		page, err := store.ListUserSizeEntryCandidates(ctx, 5, sizeEntryCandidateQuery(100), nil)
		if err != nil || page == nil || len(page.Entries) != 100 || !page.HasMore || page.Entries[0].ID != 1100 || page.Entries[99].ID != 1001 {
			t.Fatal("大小百条页边界错误", err)
		}
		next, err := store.ListUserSizeEntryCandidates(ctx, 5, sizeEntryCandidateQuery(100), &domain.SizeEntryPosition{ID: 1001})
		if err != nil {
			t.Fatal(err)
		}
		check(t, next, []uint64{1000}, false)
	})
	t.Run("unsigned size and ID", func(t *testing.T) {
		maximum := ^uint64(0)
		for _, statement := range []string{
			fmt.Sprintf("INSERT INTO users(id,username,display_name,password_hash) VALUES(%d,'fixture_size_max','最大大小用户','fixture_hash')", maximum),
			fmt.Sprintf("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(%d,%d,'MINIO','fixture-bucket','fixture-size-max',%d)", maximum, maximum, maximum),
			fmt.Sprintf("INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(%d,%d,NULL,'',X'','DIR'),(%d,%d,%d,'zero',CAST('zero' AS BINARY),'DIR')", maximum-3, maximum, maximum-2, maximum, maximum-3),
		} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		for _, fixture := range []struct{ entryID, size uint64 }{{maximum, maximum}, {maximum - 1, 1 << 63}} {
			name := fmt.Sprintf("max-%d", fixture.entryID)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(?,?,?,?,?,'FILE','OTHER',?,?,?)", fixture.entryID, maximum, maximum-3, name, []byte(name), maximum, fixture.size, maximum); err != nil {
				t.Fatal(err)
			}
		}
		var before *domain.SizeEntryPosition
		for index, entryID := range []uint64{maximum, maximum - 1, maximum - 2} {
			page, err := store.ListUserSizeEntryCandidates(ctx, maximum, sizeEntryCandidateQuery(1), before)
			if err != nil {
				t.Fatal(err)
			}
			check(t, page, []uint64{entryID}, index < 2)
			last := page.Entries[0]
			before = &domain.SizeEntryPosition{FileSize: last.FileSize, ID: last.ID}
		}
		listing := service.NewSizeEntryListService(func(current context.Context, action func(context.Context, service.SizeEntryListReader) error) error {
			return store.WithFileEntrySnapshot(current, func(snapshotContext context.Context, reader *Store) error {
				return action(snapshotContext, reader)
			})
		})
		result, err := listing.List(ctx, domain.Actor{UserID: maximum, DeviceID: 19, SessionID: 17}, sizeEntryCandidateQuery(100), nil)
		if err != nil || result == nil || len(result.Entries) != 3 || result.HasMore || result.NextPosition != nil || result.Entries[0].FileSize != maximum || result.Entries[1].FileSize != 1<<63 || result.Entries[2].FileSize != 0 {
			t.Fatal("大小可见列表完整无符号边界错误", err)
		}
		signer, err := service.NewSizeEntryCursorSigner([]byte(strings.Repeat("s", 32)))
		if err != nil {
			t.Fatal(err)
		}
		verifier, err := service.NewSizeEntryCursorVerifier([]byte(strings.Repeat("s", 32)))
		if err != nil {
			t.Fatal(err)
		}
		paging := service.NewSizeEntryPageService(verifier.Verify, listing.List, signer.Sign)
		cursor := ""
		for index, entryID := range []uint64{maximum, maximum - 1, maximum - 2} {
			page, err := paging.List(ctx, domain.Actor{UserID: maximum, DeviceID: 19, SessionID: 17}, sizeEntryCandidateQuery(1), cursor)
			if err != nil || page == nil || len(page.Entries) != 1 || page.Entries[0].ID != entryID || !reflect.DeepEqual(page.Entries[0], result.Entries[index]) || (page.NextCursor != nil) != (index < 2) {
				t.Fatal("大小文本分页完整无符号边界错误", err)
			}
			if page.NextCursor != nil {
				cursor = *page.NextCursor
			}
		}
	})
}
