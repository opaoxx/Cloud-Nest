package repository

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestListUserGlobalEntryCandidatesSearchIntegration(t *testing.T) {
	// 1. 在正式表中准备名称、分类、用户及生命周期夹具。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','搜索候选用户','fixture_hash'),(3,'fixture_other','其他用户','fixture_hash'),(5,'fixture_empty','空用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-search-object',2048),(43,3,'MINIO','fixture-bucket','fixture-other-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(55,5,NULL,'',X'','DIR'),(12,1,11,'学习 资料',CAST('学习 资料' AS BINARY),'DIR'),(14,1,12,'另一个课程',CAST('另一个课程' AS BINARY),'DIR'),(16,1,11,'回收目录',CAST('回收目录' AS BINARY),'DIR')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立名称搜索夹具失败", err)
		}
	}
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	for _, fixture := range []struct {
		id, userID, parentID, objectID uint64
		name, category                 string
		seconds                        int
	}{
		{101, 1, 12, 41, "学习笔记.md", "DOCUMENT", 3},
		{102, 1, 14, 41, "学习计划.md", "DOCUMENT", 3},
		{103, 1, 11, 41, "学习截图.png", "IMAGE", 2},
		{104, 1, 11, 41, "100%完成.md", "DOCUMENT", 1},
		{105, 1, 11, 41, "1000完成.md", "DOCUMENT", 0},
		{106, 1, 11, 41, "a_b.md", "DOCUMENT", 0},
		{107, 1, 11, 41, "axb.md", "DOCUMENT", 0},
		{108, 1, 11, 41, "等号=记录.md", "DOCUMENT", 0},
		{109, 1, 11, 41, "反斜线\\记录.md", "DOCUMENT", 0},
		{110, 1, 11, 41, "引号'记录.md", "DOCUMENT", 0},
		{111, 1, 11, 41, "双引号\"记录.md", "DOCUMENT", 0},
		{112, 1, 11, 41, "问号?记录.md", "DOCUMENT", 0},
		{113, 1, 11, 41, "Café.md", "DOCUMENT", 0},
		{114, 1, 11, 41, "空格 尾部 .md", "DOCUMENT", 0},
		{115, 1, 11, 41, "emoji🌱.md", "IMAGE", 0},
		{116, 1, 16, 41, "学习祖先下.md", "DOCUMENT", 0},
		{117, 1, 12, 41, "学习已回收.md", "DOCUMENT", 0},
		{118, 1, 12, 41, "学习清理中.md", "DOCUMENT", 0},
		{119, 1, 12, 41, "学习已清理.md", "DOCUMENT", 0},
		{120, 1, 11, 41, "组合=%_\\记录.md", "ARCHIVE", 0},
		{121, 1, 11, 41, "' OR TRUE --.md", "OTHER", 0},
		{122, 1, 11, 41, "NUL\x00记录.md", "OTHER", 0},
		{201, 3, 33, 43, "学习其他用户.md", "DOCUMENT", 0},
	} {
		_, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,modified_at) VALUES(?,?,?,?,?,'FILE',?,?,2048,?)", fixture.id, fixture.userID, fixture.parentID, fixture.name, []byte(fixture.name), fixture.category, fixture.objectID, stamp.Add(time.Duration(fixture.seconds)*time.Second))
		if err != nil {
			t.Fatal("建立搜索名称记录失败", err)
		}
	}
	for _, statement := range []string{
		"UPDATE file_entries SET modified_at='2026-11-15 02:30:00.123456' WHERE entry_type='DIR'",
		"UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id IN (16,117)",
		"UPDATE file_entries SET status='PURGING',purge_started_at=UTC_TIMESTAMP(6) WHERE id=118",
		"UPDATE file_entries SET status='PURGED',purged_at=UTC_TIMESTAMP(6),object_id=NULL WHERE id=119",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	checkPage := func(t *testing.T, page *domain.FileEntryPage, ids []uint64, more bool) {
		t.Helper()
		if page == nil || page.Entries == nil || len(page.Entries) != len(ids) || page.HasMore != more {
			t.Fatal("搜索页长度、空切片或后续页提示错误")
		}
		for index, entry := range page.Entries {
			if entry.ID != ids[index] || entry.ParentID == nil || entry.Status != "ACTIVE" {
				t.Fatal("搜索候选顺序或范围错误")
			}
		}
	}

	// 2. 验证包含匹配、特殊字符及分类组合，并逐条核对基础记录。
	for _, fixture := range []struct {
		name, search, kind string
		ids                []uint64
	}{
		{"Chinese across parents", "学习", "", []uint64{102, 101, 103, 116, 12}},
		{"Chinese documents", "学习", "document", []uint64{102, 101, 116}},
		{"Chinese folders", "学习", "folder", []uint64{12}},
		{"Chinese images", "学习", "image", []uint64{103}},
		{"category intersection empty", "学习", "other", nil},
		{"literal percent", "%", "", []uint64{104, 120}},
		{"literal underscore", "_", "", []uint64{120, 106}},
		{"literal escape", "=", "", []uint64{120, 108}},
		{"literal backslash", "\\", "", []uint64{120, 109}},
		{"combined escapes", "=%_\\", "archive", []uint64{120}},
		{"literal single quote", "'", "", []uint64{121, 110}},
		{"literal double quote", "\"", "", []uint64{111}},
		{"literal placeholder", "?", "", []uint64{112}},
		{"literal SQL text", "' OR TRUE --", "", []uint64{121}},
		{"case and accent collation", "cafe", "", []uint64{113}},
		{"Unicode emoji", "🌱", "", []uint64{115}},
		{"preserved whitespace", " 尾部 ", "", []uint64{114}},
		{"leading whitespace significant", "  学习 ", "", nil},
		{"embedded zero", "\x00", "", []uint64{122}},
		{"long keyword", strings.Repeat("界", 1024), "", nil},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			query := globalEntryCandidateQuery(100)
			query.Search, query.Kind = fixture.search, fixture.kind
			before := query
			page, err := store.ListUserGlobalEntryCandidates(ctx, 1, query, nil)
			if err != nil || query != before {
				t.Fatal("搜索失败或查询原值改变", err)
			}
			checkPage(t, page, fixture.ids, false)
			for _, entry := range page.Entries {
				stored, err := store.FindUserFileEntry(ctx, 1, entry.ID)
				if err != nil || stored == nil || !reflect.DeepEqual(entry, *stored) {
					t.Fatal("搜索结果基础记录错误", err)
				}
			}
		})
	}

	// 3. 验证搜索范围、复合位置分页与名称变更快照。
	t.Run("filtered tuple pagination", func(t *testing.T) {
		query := globalEntryCandidateQuery(1)
		query.Search, query.Kind = "学习", "document"
		var before *domain.FileEntryPosition
		for index, id := range []uint64{102, 101, 116} {
			page, err := store.ListUserGlobalEntryCandidates(ctx, 1, query, before)
			if err != nil {
				t.Fatal(err)
			}
			checkPage(t, page, []uint64{id}, index < 2)
			last := page.Entries[0]
			before = &domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID}
		}
		empty, err := store.ListUserGlobalEntryCandidates(ctx, 1, query, before)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, empty, nil, false)
	})
	t.Run("ownership and empty results", func(t *testing.T) {
		query := globalEntryCandidateQuery(100)
		query.Search, query.Kind = "学习", "document"
		for _, userID := range []uint64{5, 999} {
			empty, err := store.ListUserGlobalEntryCandidates(ctx, userID, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkPage(t, empty, nil, false)
		}
		foreign, err := store.ListUserGlobalEntryCandidates(ctx, 3, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, foreign, []uint64{201}, false)
	})
	t.Run("rename respects search snapshot", func(t *testing.T) {
		query := globalEntryCandidateQuery(1)
		query.Search, query.Kind = "学习", "document"
		if err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			first, err := txStore.ListUserGlobalEntryCandidates(txContext, 1, query, nil)
			if err != nil {
				return err
			}
			checkPage(t, first, []uint64{102}, true)
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name=?,name_key=?,modified_at=?,updated_at=? WHERE id=102", "移出搜索.md", []byte("移出搜索.md"), stamp.Add(20*time.Second), stamp.Add(20*time.Second)); err != nil {
				return err
			}
			again, err := txStore.ListUserGlobalEntryCandidates(txContext, 1, query, nil)
			if err != nil || !reflect.DeepEqual(first, again) {
				t.Fatal("搜索快照混入后续名称修改", err)
			}
			last := first.Entries[0]
			next, err := txStore.ListUserGlobalEntryCandidates(txContext, 1, query, &domain.FileEntryPosition{ModifiedAt: last.ModifiedAt, ID: last.ID})
			if err != nil {
				return err
			}
			checkPage(t, next, []uint64{101}, true)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		query.PageSize = 100
		current, err := store.ListUserGlobalEntryCandidates(ctx, 1, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, current, []uint64{101, 116}, false)
	})

	// 4. 在单连接中验证显式转义与 SQL 模式，并恢复连接设置。
	t.Run("explicit escape under no backslash mode", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		var originalMode string
		if err := pool.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode").Scan(&originalMode); err != nil {
			t.Fatal(err)
		}
		defer func() {
			restoreContext, release := context.WithTimeout(context.Background(), 3*time.Second)
			defer release()
			if _, err := pool.ExecContext(restoreContext, "SET SESSION sql_mode = ?", originalMode); err != nil {
				t.Error("恢复测试连接 SQL 模式失败", err)
			}
			pool.SetMaxOpenConns(2)
		}()
		mode := originalMode
		if !strings.Contains(","+mode+",", ",NO_BACKSLASH_ESCAPES,") {
			mode += ",NO_BACKSLASH_ESCAPES"
		}
		if _, err := pool.ExecContext(ctx, "SET SESSION sql_mode = ?", mode); err != nil {
			t.Fatal(err)
		}
		for _, fixture := range []struct {
			search string
			ids    []uint64
		}{
			{"%", []uint64{104, 120}},
			{"_", []uint64{120, 106}},
			{"=", []uint64{120, 108}},
			{"\\", []uint64{120, 109}},
			{"=%_\\", []uint64{120}},
			{"' OR TRUE --", []uint64{121}},
		} {
			query := globalEntryCandidateQuery(100)
			query.Search = fixture.search
			page, err := store.ListUserGlobalEntryCandidates(ctx, 1, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkPage(t, page, fixture.ids, false)
		}
	})
}
