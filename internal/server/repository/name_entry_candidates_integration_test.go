package repository

import (
	"bytes"
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

func TestListUserNameEntryCandidatesIntegration(t *testing.T) {
	// 1. 复用独占库和正式表，准备跨目录同键、隐藏路径及收藏交集。
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
		t.Fatal("未找到偏好表结束边界")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:start+end]); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_name_one','名称用户','fixture_hash'),(3,'fixture_name_other','其他名称用户','fixture_hash'),(5,'fixture_name_batch','名称批次用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-name',2048),(43,3,'MINIO','fixture-bucket','fixture-name-other',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(55,5,NULL,'',X'','DIR'),(12,1,11,'Parent-one',X'706172656e742d6f6e65','DIR'),(14,1,11,'Parent-two',X'706172656e742d74776f','DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(16,1,11,'Trashed-parent',CAST('trashed-parent' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(101,1,12,'ReadMe.MD',CAST('readme.md' AS BINARY),'FILE','DOCUMENT',41,2048),(103,1,14,'readme.md',CAST('readme.md' AS BINARY),'FILE','DOCUMENT',41,2048),(105,1,12,'a%_.png',CAST('a%_.png' AS BINARY),'FILE','IMAGE',41,2048),(109,1,12,'Z.bin',CAST('z.bin' AS BINARY),'FILE','OTHER',41,2048),(111,1,16,'b-hidden.md',CAST('b-hidden.md' AS BINARY),'FILE','DOCUMENT',41,2048),(121,3,33,'ReadMe.MD',CAST('readme.md' AS BINARY),'FILE','DOCUMENT',43,2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(107,1,12,'Docs',CAST('docs' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(113,1,12,'A-trash',CAST('a-trash' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at) VALUES(115,1,12,'A-purge',CAST('a-purge' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purged_at) VALUES(117,1,12,'A-purged',CAST('a-purged' AS BINARY),'DIR','PURGED',UTC_TIMESTAMP(6))",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,11,UTC_TIMESTAMP(6),NULL),(1,101,UTC_TIMESTAMP(6),NULL),(1,103,UTC_TIMESTAMP(6),NULL),(1,107,UTC_TIMESTAMP(6),NULL),(1,111,UTC_TIMESTAMP(6),NULL),(1,105,NULL,UTC_TIMESTAMP(6))",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Date(2026, 11, 17, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", stamp.Add(-time.Hour), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, page *domain.NameEntryPage, ids []uint64, more bool) {
		t.Helper()
		if page == nil || page.Entries == nil || len(page.Entries) != len(ids) || page.HasMore != more {
			t.Fatal("名称候选页形状错误")
		}
		for index, candidate := range page.Entries {
			stored, err := store.FindUserFileEntry(ctx, candidate.Entry.UserID, candidate.Entry.ID)
			var key []byte
			keyError := pool.QueryRowContext(ctx, "SELECT name_key FROM file_entries WHERE user_id=? AND id=?", candidate.Entry.UserID, candidate.Entry.ID).Scan(&key)
			if err != nil || keyError != nil || stored == nil || candidate.Entry.ID != ids[index] || !reflect.DeepEqual(candidate.Entry, *stored) || !bytes.Equal(candidate.NameKey, key) {
				t.Fatal("名称候选十二列与数据库事实不一致", err, keyError)
			}
		}
	}

	// 2. 验证跨目录同键的 ID 升序、原始键和值，以及收藏范围。
	for _, scope := range []string{"all", "starred"} {
		t.Run("pagination "+scope, func(t *testing.T) {
			query := nameEntryCandidateQuery(2)
			query.Scope = scope
			expected := []uint64{105, 111, 107, 12, 14, 101, 103, 109}
			if scope == "starred" {
				expected = []uint64{111, 107, 101, 103}
			}
			var after *domain.NameEntryPosition
			var found []uint64
			for offset := 0; offset < len(expected); offset += 2 {
				page, err := store.ListUserNameEntryCandidates(ctx, 1, query, after)
				if err != nil {
					t.Fatal(err)
				}
				check(t, page, expected[offset:min(offset+2, len(expected))], offset+2 < len(expected))
				for _, candidate := range page.Entries {
					found = append(found, candidate.Entry.ID)
				}
				last := page.Entries[len(page.Entries)-1]
				after = &domain.NameEntryPosition{NameKey: bytes.Clone(last.NameKey), ID: last.Entry.ID}
			}
			if !reflect.DeepEqual(found, expected) {
				t.Fatal("名称翻页漏项或重复")
			}
			page, err := store.ListUserNameEntryCandidates(ctx, 1, query, after)
			if err != nil {
				t.Fatal(err)
			}
			check(t, page, nil, false)
		})
	}
	t.Run("scope filters and literal search", func(t *testing.T) {
		for _, testCase := range []struct {
			scope, kind, search string
			userID              uint64
			ids                 []uint64
		}{
			{scope: "all", userID: 3, ids: []uint64{121}},
			{scope: "all", userID: 999},
			{scope: "all", userID: 5},
			{scope: "all", userID: 1, kind: "folder", ids: []uint64{107, 12, 14}},
			{scope: "all", userID: 1, kind: "document", search: "readme", ids: []uint64{101, 103}},
			{scope: "starred", userID: 1, kind: "document", ids: []uint64{111, 101, 103}},
			{scope: "starred", userID: 1, kind: "image"},
			{scope: "all", userID: 1, kind: "image", search: "%_", ids: []uint64{105}},
			{scope: "all", userID: 1, kind: "other", ids: []uint64{109}},
			{scope: "all", userID: 1, kind: "video"},
			{scope: "all", userID: 1, kind: "audio"},
			{scope: "all", userID: 1, kind: "archive"},
		} {
			query := nameEntryCandidateQuery(100)
			query.Scope, query.Kind, query.Search = testCase.scope, testCase.kind, testCase.search
			page, err := store.ListUserNameEntryCandidates(ctx, testCase.userID, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			check(t, page, testCase.ids, false)
		}
	})
	t.Run("generated Unicode and raw binary keys", func(t *testing.T) {
		for _, entry := range []struct {
			id, parent uint64
			name       string
		}{{201, 12, "Café.md"}, {203, 14, "Cafe\u0301.MD"}} {
			key, err := service.BuildEntryNameKey(entry.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,?,?,?,'DIR')", entry.id, entry.parent, entry.name, key); err != nil {
				t.Fatal(err)
			}
		}
		for _, entry := range []struct {
			id  uint64
			key []byte
		}{{205, []byte{0}}, {207, []byte{'a', 0}}, {209, bytes.Repeat([]byte{0xff}, 768)}} {
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,12,'二进制边界',?,'DIR')", entry.id, entry.key); err != nil {
				t.Fatal(err)
			}
		}
		page, err := store.ListUserNameEntryCandidates(ctx, 1, nameEntryCandidateQuery(100), nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, page, []uint64{205, 207, 105, 111, 201, 203, 107, 12, 14, 101, 103, 109, 209}, false)
		last := page.Entries[len(page.Entries)-1]
		end, err := store.ListUserNameEntryCandidates(ctx, 1, nameEntryCandidateQuery(100), &domain.NameEntryPosition{NameKey: last.NameKey, ID: last.Entry.ID})
		if err != nil {
			t.Fatal(err)
		}
		check(t, end, nil, false)
		if _, err := pool.ExecContext(ctx, "DELETE FROM file_entries WHERE id BETWEEN 201 AND 209"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("independent results and positions", func(t *testing.T) {
		position := &domain.NameEntryPosition{NameKey: []byte("readme.md"), ID: 101}
		original := bytes.Clone(position.NameKey)
		page, err := store.ListUserNameEntryCandidates(ctx, 1, nameEntryCandidateQuery(1), position)
		if err != nil {
			t.Fatal(err)
		}
		check(t, page, []uint64{103}, true)
		page.Entries[0].NameKey[0] = 'x'
		*page.Entries[0].Entry.ParentID = 999
		again, err := store.ListUserNameEntryCandidates(ctx, 1, nameEntryCandidateQuery(1), position)
		if err != nil || !bytes.Equal(position.NameKey, original) || position.ID != 101 {
			t.Fatal("名称来源位置被改写", err)
		}
		check(t, again, []uint64{103}, true)
	})
	// 3. 在同一快照中验证外部已提交改名，并核对未提交改名可见性。
	t.Run("snapshot keeps name key across rename", func(t *testing.T) {
		err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, reader *Store) error {
			first, err := reader.ListUserNameEntryCandidates(snapshotContext, 1, nameEntryCandidateQuery(100), nil)
			if err != nil {
				return err
			}
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name='AA.md',name_key=CAST('aa.md' AS BINARY) WHERE id=103"); err != nil {
				return err
			}
			second, err := reader.ListUserNameEntryCandidates(snapshotContext, 1, nameEntryCandidateQuery(100), nil)
			if err != nil || !reflect.DeepEqual(first, second) {
				return fmt.Errorf("名称快照未保留原始键: %w", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		page, err := store.ListUserNameEntryCandidates(ctx, 1, nameEntryCandidateQuery(100), nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, page, []uint64{105, 103, 111, 107, 12, 14, 101, 109}, false)
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name='readme.md',name_key=CAST('readme.md' AS BINARY) WHERE id=103"); err != nil {
			t.Fatal(err)
		}
		writer, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		if _, err := writer.ExecContext(ctx, "UPDATE file_entries SET name='AA.md',name_key=CAST('aa.md' AS BINARY) WHERE id=103"); err != nil {
			t.Fatal(err)
		}
		readContext, release := context.WithTimeout(ctx, 500*time.Millisecond)
		defer release()
		page, err = store.ListUserNameEntryCandidates(readContext, 1, nameEntryCandidateQuery(100), nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, page, []uint64{105, 111, 107, 12, 14, 101, 103, 109}, false)
	})
	t.Run("maximum page and unsigned position", func(t *testing.T) {
		for index := 0; index < 101; index++ {
			name := fmt.Sprintf("batch-%03d", index)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,5,55,?,?,'DIR')", 1000+index, name, []byte(name)); err != nil {
				t.Fatal(err)
			}
		}
		first, err := store.ListUserNameEntryCandidates(ctx, 5, nameEntryCandidateQuery(100), nil)
		if err != nil || first == nil || len(first.Entries) != 100 || !first.HasMore || first.Entries[0].Entry.ID != 1000 || first.Entries[99].Entry.ID != 1099 {
			t.Fatal("名称百条页边界错误", err)
		}
		last := first.Entries[99]
		next, err := store.ListUserNameEntryCandidates(ctx, 5, nameEntryCandidateQuery(100), &domain.NameEntryPosition{NameKey: last.NameKey, ID: last.Entry.ID})
		if err != nil {
			t.Fatal(err)
		}
		check(t, next, []uint64{1100}, false)
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_name_max','完整名称用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum-1, maximum); err != nil {
			t.Fatal(err)
		}
		key := bytes.Repeat([]byte{0xff}, 768)
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,revision) VALUES(?,?,?,'最大名称',?,'DIR',?)", maximum, maximum, maximum-1, key, maximum); err != nil {
			t.Fatal(err)
		}
		page, err := store.ListUserNameEntryCandidates(ctx, maximum, nameEntryCandidateQuery(1), &domain.NameEntryPosition{NameKey: []byte{0xff}, ID: maximum})
		if err != nil {
			t.Fatal(err)
		}
		check(t, page, []uint64{maximum}, false)
	})
}
