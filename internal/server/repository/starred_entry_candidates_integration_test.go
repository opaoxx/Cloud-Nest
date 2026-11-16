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

func TestListUserStarredEntryCandidatesIntegration(t *testing.T) {
	// 1. 复用独占正式表，增加偏好表及收藏、访问、生命周期夹具。
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
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','收藏候选用户','fixture_hash'),(3,'fixture_three','其他用户','fixture_hash'),(5,'fixture_empty','空用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-starred-candidates',2048),(43,3,'MINIO','fixture-bucket','fixture-starred-foreign',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(55,5,NULL,'',X'','DIR'),(12,1,11,'目录一',CAST('目录一' AS BINARY),'DIR'),(14,1,11,'目录二',CAST('目录二' AS BINARY),'DIR'),(106,1,12,'资料',CAST('资料' AS BINARY),'DIR'),(107,1,12,'仅访问目录',CAST('仅访问目录' AS BINARY),'DIR'),(108,1,12,'空偏好目录',CAST('空偏好目录' AS BINARY),'DIR'),(109,1,12,'无偏好目录',CAST('无偏好目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(16,1,11,'回收祖先',CAST('回收祖先' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6)),(110,1,12,'回收收藏',CAST('回收收藏' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at) VALUES(111,1,12,'清理收藏',CAST('清理收藏' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purged_at) VALUES(112,1,12,'清理结束收藏',CAST('清理结束收藏' AS BINARY),'DIR','PURGED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(101,1,12,'学习%_笔记.md',CAST('学习%_笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048,3),(103,1,14,'学习%_计划.md',CAST('学习%_计划.md' AS BINARY),'FILE','DOCUMENT',41,2048,1),(104,1,12,'学习%_未收藏.md',CAST('学习%_未收藏.md' AS BINARY),'FILE','DOCUMENT',41,2048,1),(105,1,12,'风景%_.png',CAST('风景%_.png' AS BINARY),'FILE','IMAGE',41,2048,1),(113,1,16,'学习%_隐藏.md',CAST('学习%_隐藏.md' AS BINARY),'FILE','DOCUMENT',41,2048,1),(121,3,33,'学习%_其他.md',CAST('学习%_其他.md' AS BINARY),'FILE','DOCUMENT',43,2048,1)",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,11,'2020-01-02 03:04:05.123456',NULL),(1,101,'2022-01-02 03:04:05.123456','2023-01-02 03:04:05.123456'),(1,103,'2021-01-02 03:04:05.123456',NULL),(1,104,NULL,'2023-01-02 03:04:05.123456'),(1,105,'2020-01-02 03:04:05.123456',NULL),(1,106,'2020-01-02 03:04:05.123456',NULL),(1,107,NULL,'2023-01-02 03:04:05.123456'),(1,108,NULL,NULL),(1,110,'2020-01-02 03:04:05.123456',NULL),(1,111,'2020-01-02 03:04:05.123456',NULL),(1,112,'2020-01-02 03:04:05.123456',NULL),(1,113,'2020-01-02 03:04:05.123456',NULL),(3,33,'2020-01-02 03:04:05.123456',NULL),(3,121,'2020-01-02 03:04:05.123456',NULL)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立收藏候选夹具失败", err)
		}
	}
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", stamp.Add(-time.Hour), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=TIMESTAMPADD(SECOND,IF(id=103,2,1),?) WHERE id IN (101,103)", stamp); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	query := globalEntryCandidateQuery(100)
	query.Scope = domain.GlobalEntryScopeStarred
	checkIDs := func(t *testing.T, page *domain.FileEntryPage, expected []uint64, more bool) {
		t.Helper()
		if page == nil || page.Entries == nil || len(page.Entries) != len(expected) || page.HasMore != more {
			t.Fatal("收藏候选页形状或续页状态错误")
		}
		for index, entry := range page.Entries {
			if entry.ID != expected[index] {
				t.Fatal("收藏候选范围或修改时间排序错误", entry.ID, expected[index])
			}
		}
	}

	// 2. 验证用户/收藏/活动状态和原文搜索、分类交集。
	for _, testCase := range []struct {
		name, kind, search string
		userID             uint64
		ids                []uint64
	}{
		{name: "starred only", userID: 1, ids: []uint64{103, 101, 113, 106, 105}},
		{name: "document", userID: 1, kind: "document", ids: []uint64{103, 101, 113}},
		{name: "folder", userID: 1, kind: "folder", ids: []uint64{106}},
		{name: "image", userID: 1, kind: "image", ids: []uint64{105}},
		{name: "search literal", userID: 1, search: "学习%_", ids: []uint64{103, 101, 113}},
		{name: "document intersection", userID: 1, search: "学习%_", kind: "document", ids: []uint64{103, 101, 113}},
		{name: "folder intersection", userID: 1, search: "资料", kind: "folder", ids: []uint64{106}},
		{name: "percent underscore literal", userID: 1, search: "%_", ids: []uint64{103, 101, 113, 105}},
		{name: "no matching category", userID: 1, kind: "audio"},
		{name: "no matching search", userID: 1, search: "不存在"},
		{name: "foreign user", userID: 3, ids: []uint64{121}},
		{name: "empty user", userID: 5},
		{name: "missing user", userID: 999},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			filters := query
			filters.Search, filters.Kind = testCase.search, testCase.kind
			page, err := store.ListUserGlobalEntryCandidates(ctx, testCase.userID, filters, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkIDs(t, page, testCase.ids, false)
			for _, entry := range page.Entries {
				stored, err := store.FindUserFileEntry(ctx, testCase.userID, entry.ID)
				if err != nil || stored == nil || !reflect.DeepEqual(entry, *stored) {
					t.Fatal("收藏候选基础字段与数据库不一致", err)
				}
			}
		})
	}
	t.Run("pagination after favorite filtering", func(t *testing.T) {
		filters := query
		filters.PageSize = 2
		var before *domain.FileEntryPosition
		for index, expected := range [][]uint64{{103, 101}, {113, 106}, {105}, {}} {
			page, err := store.ListUserGlobalEntryCandidates(ctx, 1, filters, before)
			if err != nil {
				t.Fatal(err)
			}
			checkIDs(t, page, expected, index < 2)
			if len(page.Entries) != 0 {
				last := page.Entries[len(page.Entries)-1]
				before = &domain.FileEntryPosition{ID: last.ID, ModifiedAt: last.ModifiedAt}
			}
		}
	})
	t.Run("shared visibility in one snapshot and one connection", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		err := store.WithFileEntrySnapshot(ctx, func(current context.Context, reader *Store) error {
			page, err := reader.ListUserGlobalEntryCandidates(current, 1, query, nil)
			if err != nil {
				return err
			}
			checkIDs(t, page, []uint64{103, 101, 113, 106, 105}, false)
			visible, err := service.FilterVisibleFileEntries(current, domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}, page.Entries, reader)
			if err != nil {
				return err
			}
			checkIDs(t, &domain.FileEntryPage{Entries: visible}, []uint64{103, 101, 106, 105}, false)
			return nil
		})
		if err != nil {
			t.Fatal("收藏候选与完整父链检查衔接失败", err)
		}
	})

	t.Run("maximum page after favorite filtering", func(t *testing.T) {
		writer, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		for index := 0; index < 102; index++ {
			name := fmt.Sprintf("收藏批次-%d", index)
			if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,modified_at) VALUES(?,5,55,?,?,'DIR',?)", 1000+index, name, []byte(name), stamp); err != nil {
				t.Fatal(err)
			}
			if index < 101 {
				if _, err := writer.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) VALUES(5,?,?)", 1000+index, stamp); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := writer.Commit(); err != nil {
			t.Fatal(err)
		}
		page, err := store.ListUserGlobalEntryCandidates(ctx, 5, query, nil)
		if err != nil || page == nil || len(page.Entries) != 100 || !page.HasMore || page.Entries[0].ID != 1100 || page.Entries[99].ID != 1001 {
			t.Fatal("百条收藏页或筛选后的额外候选错误", err)
		}
		end, err := store.ListUserGlobalEntryCandidates(ctx, 5, query, &domain.FileEntryPosition{ID: 1001, ModifiedAt: stamp})
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, end, []uint64{1000}, false)
	})

	// 3. 验证偏好提交边界、快照一致性及读取结果独立。
	t.Run("uncommitted favorites", func(t *testing.T) {
		writer, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		if _, err := writer.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=IF(entry_id=101,NULL,UTC_TIMESTAMP(6)) WHERE user_id=1 AND entry_id IN (101,104)"); err != nil {
			t.Fatal(err)
		}
		page, err := store.ListUserGlobalEntryCandidates(ctx, 1, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, page, []uint64{103, 101, 113, 106, 105}, false)
	})
	t.Run("favorite changes preserve existing snapshot", func(t *testing.T) {
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=IF(entry_id=101,'2022-01-02 03:04:05.123456',NULL) WHERE user_id=1 AND entry_id IN (101,104)"); err != nil {
				t.Error("恢复收藏夹具失败", err)
			}
		}()
		err := store.WithFileEntrySnapshot(ctx, func(current context.Context, reader *Store) error {
			first, err := reader.ListUserGlobalEntryCandidates(current, 1, query, nil)
			if err != nil {
				return err
			}
			checkIDs(t, first, []uint64{103, 101, 113, 106, 105}, false)
			if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=IF(entry_id=101,NULL,UTC_TIMESTAMP(6)) WHERE user_id=1 AND entry_id IN (101,104)"); err != nil {
				return err
			}
			again, err := reader.ListUserGlobalEntryCandidates(current, 1, query, nil)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(first, again) {
				return fmt.Errorf("快照中的收藏条件发生变化")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		current, err := store.ListUserGlobalEntryCandidates(ctx, 1, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, current, []uint64{103, 113, 106, 105, 104}, false)
	})
	t.Run("copies preserve entry and preference facts", func(t *testing.T) {
		preference, err := store.FindUserEntryPreference(ctx, 1, 103)
		if err != nil {
			t.Fatal(err)
		}
		page, err := store.ListUserGlobalEntryCandidates(ctx, 1, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		page.Entries[0].Name, *page.Entries[0].ParentID = "本地修改", 999
		again, err := store.ListUserGlobalEntryCandidates(ctx, 1, query, nil)
		if err != nil || again.Entries[0].Name != "学习%_计划.md" || *again.Entries[0].ParentID != 14 {
			t.Fatal("修改读取副本影响了后续候选", err)
		}
		after, err := store.FindUserEntryPreference(ctx, 1, 103)
		if err != nil || !reflect.DeepEqual(preference, after) {
			t.Fatal("收藏候选读取修改了偏好事实", err)
		}
	})

	// 4. 完整无符号用户/目录项/版本与数据库日期边界最后插入。
	t.Run("maximum identifiers and database dates", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','最大用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum-1, maximum); err != nil {
			t.Fatal(err)
		}
		minimumTime := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
		maximumTime := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,revision,created_at,modified_at,updated_at) VALUES(?,?,?,'最大目录',CAST('最大目录' AS BINARY),'DIR',?,?,?,?)", maximum, maximum, maximum-1, maximum, minimumTime, maximumTime, maximumTime); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) VALUES(?,?,?)", maximum, maximum, minimumTime); err != nil {
			t.Fatal(err)
		}
		page, err := store.ListUserGlobalEntryCandidates(ctx, maximum, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, page, []uint64{maximum}, false)
		entry := page.Entries[0]
		if entry.UserID != maximum || *entry.ParentID != maximum-1 || entry.Revision != maximum || !entry.CreatedAt.Equal(minimumTime) || !entry.ModifiedAt.Equal(maximumTime) {
			t.Fatal("收藏范围的完整整数或日期精度错误")
		}
		end, err := store.ListUserGlobalEntryCandidates(ctx, maximum, query, &domain.FileEntryPosition{ID: maximum, ModifiedAt: maximumTime})
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, end, nil, false)
	})
}
