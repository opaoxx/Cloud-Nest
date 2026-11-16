package repository

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// verifyRecentEntryCandidateSearch 复用最近候选的正式表和基础夹具，验证名称搜索。
func verifyRecentEntryCandidateSearch(t *testing.T, ctx context.Context, pool *sql.DB, store *Store, stamp time.Time) {
	t.Helper()
	// 1. 增加独占名称夹具，保留访问、分类、用户及生命周期交集。
	defer func() {
		for _, statement := range []string{
			"DELETE FROM user_entry_preferences WHERE entry_id BETWEEN 130 AND 153",
			"DELETE FROM file_entries WHERE id BETWEEN 130 AND 153",
		} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Error("清理最近访问搜索夹具失败", err)
			}
		}
	}()
	for _, fixture := range []struct {
		id, userID, parentID, objectID uint64
		name, category                 string
		seconds                        int
	}{
		{130, 1, 12, 41, "检索笔记.md", "DOCUMENT", 3},
		{131, 1, 14, 41, "检索计划.md", "DOCUMENT", 3},
		{132, 1, 12, 41, "检索截图.png", "IMAGE", 2},
		{133, 1, 12, 41, "100%完成.md", "DOCUMENT", 0},
		{134, 1, 12, 41, "1000完成.md", "DOCUMENT", 0},
		{135, 1, 12, 41, "a_b.md", "DOCUMENT", 0},
		{136, 1, 12, 41, "axb.md", "DOCUMENT", 0},
		{137, 1, 12, 41, "等号=记录.md", "DOCUMENT", 0},
		{138, 1, 12, 41, "反斜线\\记录.md", "DOCUMENT", 0},
		{139, 1, 12, 41, "引号'记录.md", "DOCUMENT", 0},
		{140, 1, 12, 41, "问号?记录.md", "OTHER", 0},
		{141, 1, 12, 41, "Café.md", "DOCUMENT", 0},
		{142, 1, 12, 41, "空格 尾部 .md", "DOCUMENT", 0},
		{143, 1, 12, 41, "emoji🐾.md", "IMAGE", 0},
		{144, 1, 12, 41, "组合=%_\\记录.md", "ARCHIVE", 0},
		{145, 1, 12, 41, "' OR TRUE --.md", "OTHER", 0},
		{146, 1, 12, 41, "NUL\x00记录.md", "OTHER", 0},
		{147, 3, 33, 43, "检索其他用户.md", "DOCUMENT", 3},
		{149, 1, 16, 41, "检索隐藏.md", "DOCUMENT", 1},
		{150, 1, 12, 41, "检索已回收.md", "DOCUMENT", 0},
		{151, 1, 12, 41, "检索清理中.md", "DOCUMENT", 0},
		{152, 1, 12, 41, "检索已清理.md", "DOCUMENT", 0},
		{153, 1, 12, 41, "分解-Cafe\u0301.md", "DOCUMENT", 0},
	} {
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,modified_at) VALUES(?,?,?,?,?,'FILE',?,?,2048,?)", fixture.id, fixture.userID, fixture.parentID, fixture.name, []byte(fixture.name), fixture.category, fixture.objectID, stamp); err != nil {
			t.Fatal("建立最近访问搜索文件失败", err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,last_opened_at) VALUES(?,?,?)", fixture.userID, fixture.id, stamp.Add(time.Duration(fixture.seconds)*time.Second)); err != nil {
			t.Fatal("建立最近访问搜索偏好失败", err)
		}
	}
	for _, statement := range []string{
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id) VALUES(148,1,12,'检索未访问.md',CAST('检索未访问.md' AS BINARY),'FILE','DOCUMENT',41)",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) VALUES(1,148,UTC_TIMESTAMP(6))",
		"UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=150",
		"UPDATE file_entries SET status='PURGING',purge_started_at=UTC_TIMESTAMP(6) WHERE id=151",
		"UPDATE file_entries SET status='PURGED',purged_at=UTC_TIMESTAMP(6),object_id=NULL WHERE id=152",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	checkPage := func(t *testing.T, page *domain.RecentEntryPage, ids []uint64, more bool) {
		t.Helper()
		if page == nil || page.Entries == nil || len(page.Entries) != len(ids) || page.HasMore != more {
			t.Fatal("最近访问搜索页形状或续页提示错误")
		}
		for index, candidate := range page.Entries {
			if candidate.Entry.ID != ids[index] {
				t.Fatal("最近访问搜索范围或顺序错误", candidate.Entry.ID, ids[index])
			}
		}
	}

	// 2. 验证搜索原文、显式转义、分类交集及基础字段。
	for _, fixture := range []struct {
		name, search, kind string
		ids                []uint64
	}{
		{"Chinese across parents", "检索", "", []uint64{131, 130, 132, 149}},
		{"document intersection", "检索", "document", []uint64{131, 130, 149}},
		{"image intersection", "检索", "image", []uint64{132}},
		{"folder intersection", "检索", "folder", nil},
		{"empty classification intersection", "检索", "audio", nil},
		{"literal percent", "%", "", []uint64{144, 133}},
		{"literal percent prefix", "100%", "document", []uint64{133}},
		{"literal underscore", "_", "", []uint64{144, 135}},
		{"literal escape", "=", "", []uint64{144, 137}},
		{"literal backslash", "\\", "", []uint64{144, 138}},
		{"combined escapes", "=%_\\", "archive", []uint64{144}},
		{"literal quote", "'", "", []uint64{145, 139}},
		{"literal placeholder", "?", "", []uint64{140}},
		{"literal SQL text", "' OR TRUE --", "", []uint64{145}},
		{"case and accent collation", "cafe", "", []uint64{153, 141}},
		{"decomposed Unicode", "Cafe\u0301", "", []uint64{153}},
		{"Unicode emoji", "🐾", "image", []uint64{143}},
		{"preserved whitespace", " 尾部 ", "", []uint64{142}},
		{"leading whitespace significant", "  检索 ", "", nil},
		{"embedded zero under collation", "\x00", "", []uint64{153, 146}},
		{"long keyword", strings.Repeat("界", 1024), "", nil},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			query := recentEntryCandidateQuery(100)
			query.Search, query.Kind = fixture.search, fixture.kind
			original := query
			page, err := store.ListUserRecentEntryCandidates(ctx, 1, query, nil)
			if err != nil || query != original {
				t.Fatal("最近访问搜索失败或修改了输入", err)
			}
			checkPage(t, page, fixture.ids, false)
			for _, candidate := range page.Entries {
				stored, err := store.FindUserFileEntry(ctx, 1, candidate.Entry.ID)
				if err != nil || stored == nil || !reflect.DeepEqual(candidate.Entry, *stored) {
					t.Fatal("搜索结果基础字段错误", err)
				}
			}
		})
	}

	// 3. 核对搜索后的访问位置分页、用户范围及名称变更快照。
	t.Run("filtered access tuple pagination", func(t *testing.T) {
		query := recentEntryCandidateQuery(1)
		query.Search, query.Kind = "检索", "document"
		var before *domain.RecentEntryPosition
		for index, ids := range [][]uint64{{131}, {130}, {149}, {}} {
			page, err := store.ListUserRecentEntryCandidates(ctx, 1, query, before)
			if err != nil {
				t.Fatal(err)
			}
			checkPage(t, page, ids, index < 2)
			if len(page.Entries) != 0 {
				last := page.Entries[0]
				before = &domain.RecentEntryPosition{ID: last.Entry.ID, LastOpenedAt: last.LastOpenedAt}
			}
		}
		query.Kind = "image"
		page, err := store.ListUserRecentEntryCandidates(ctx, 1, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, page, []uint64{132}, false)
	})
	t.Run("ownership and empty results", func(t *testing.T) {
		query := recentEntryCandidateQuery(100)
		query.Search, query.Kind = "检索", "document"
		for _, userID := range []uint64{3, 5, 999} {
			page, err := store.ListUserRecentEntryCandidates(ctx, userID, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			var ids []uint64
			if userID == 3 {
				ids = []uint64{147}
			}
			checkPage(t, page, ids, false)
		}
	})
	t.Run("rename respects search snapshot", func(t *testing.T) {
		query := recentEntryCandidateQuery(1)
		query.Search, query.Kind = "检索", "document"
		if err := store.WithFileEntrySnapshot(ctx, func(current context.Context, reader *Store) error {
			first, err := reader.ListUserRecentEntryCandidates(current, 1, query, nil)
			if err != nil {
				return err
			}
			checkPage(t, first, []uint64{131}, true)
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name=?,name_key=?,modified_at=?,updated_at=? WHERE id=131", "移出结果.md", []byte("移出结果.md"), stamp.Add(20*time.Second), stamp.Add(20*time.Second)); err != nil {
				return err
			}
			again, err := reader.ListUserRecentEntryCandidates(current, 1, query, nil)
			if err != nil || !reflect.DeepEqual(first, again) {
				t.Fatal("名称修改影响已有最近访问搜索快照", err)
			}
			last := first.Entries[0]
			next, err := reader.ListUserRecentEntryCandidates(current, 1, query, &domain.RecentEntryPosition{ID: last.Entry.ID, LastOpenedAt: last.LastOpenedAt})
			if err != nil {
				return err
			}
			checkPage(t, next, []uint64{130}, true)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		query.PageSize = 100
		current, err := store.ListUserRecentEntryCandidates(ctx, 1, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, current, []uint64{130, 149}, false)
	})

	// 4. 在单连接验证显式转义对 SQL 模式的适用性，并恢复设置。
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
				t.Error("恢复搜索测试 SQL 模式失败", err)
			}
			pool.SetMaxOpenConns(3)
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
			{"%", []uint64{144, 133}},
			{"_", []uint64{144, 135}},
			{"=", []uint64{144, 137}},
			{"\\", []uint64{144, 138}},
			{"=%_\\", []uint64{144}},
			{"' OR TRUE --", []uint64{145}},
		} {
			query := recentEntryCandidateQuery(100)
			query.Search = fixture.search
			page, err := store.ListUserRecentEntryCandidates(ctx, 1, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkPage(t, page, fixture.ids, false)
		}
	})
}
