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

func TestListUserRecentEntryCandidatesIntegration(t *testing.T) {
	// 1. 复用独占验证库及正式表，准备访问、收藏和生命周期交集。
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
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','最近访问用户','fixture_hash'),(3,'fixture_three','其他用户','fixture_hash'),(5,'fixture_empty','空用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-recent',2048),(43,3,'MINIO','fixture-bucket','fixture-foreign',2048),(45,5,'MINIO','fixture-bucket','fixture-batch',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(55,5,NULL,'',X'','DIR'),(12,1,11,'目录一',CAST('目录一' AS BINARY),'DIR'),(14,1,11,'目录二',CAST('目录二' AS BINARY),'DIR'),(108,1,12,'历史访问目录',CAST('历史访问目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(16,1,11,'回收祖先',CAST('回收祖先' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(101,1,12,'学习笔记 🌱.md',CAST('学习笔记 🌱.md' AS BINARY),'FILE','DOCUMENT',41,2048,3),(103,1,14,'访问计划.md',CAST('访问计划.md' AS BINARY),'FILE','DOCUMENT',41,2048,1),(104,1,12,'仅收藏.md',CAST('仅收藏.md' AS BINARY),'FILE','DOCUMENT',41,2048,1),(105,1,12,'较早访问.png',CAST('较早访问.png' AS BINARY),'FILE','IMAGE',41,2048,1),(106,1,12,'无偏好.md',CAST('无偏好.md' AS BINARY),'FILE','DOCUMENT',41,2048,1),(107,1,12,'空偏好.md',CAST('空偏好.md' AS BINARY),'FILE','DOCUMENT',41,2048,1),(113,1,16,'隐藏文件.md',CAST('隐藏文件.md' AS BINARY),'FILE','DOCUMENT',41,2048,1),(121,3,33,'其他用户.md',CAST('其他用户.md' AS BINARY),'FILE','DOCUMENT',43,2048,1)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,status,deleted_at) VALUES(110,1,12,'回收文件',CAST('回收文件' AS BINARY),'FILE','OTHER',41,'TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,status,purge_started_at) VALUES(111,1,12,'清理文件',CAST('清理文件' AS BINARY),'FILE','OTHER',41,'PURGING',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,status,purged_at) VALUES(112,1,12,'已清理文件',CAST('已清理文件' AS BINARY),'FILE','OTHER','PURGED',UTC_TIMESTAMP(6))",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,11,NULL,'2020-01-02 03:04:05.123456'),(1,101,'2020-01-02 03:04:05.123456','2020-01-02 03:04:05.123456'),(1,103,NULL,'2020-01-02 03:04:05.123456'),(1,104,'2020-01-02 03:04:05.123456',NULL),(1,105,NULL,'2020-01-02 03:04:05.123456'),(1,107,NULL,NULL),(1,108,NULL,'2020-01-02 03:04:05.123456'),(1,110,NULL,'2020-01-02 03:04:05.123456'),(1,111,NULL,'2020-01-02 03:04:05.123456'),(1,112,NULL,'2020-01-02 03:04:05.123456'),(1,113,NULL,'2020-01-02 03:04:05.123456'),(3,121,NULL,'2020-01-02 03:04:05.123456')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立最近访问候选夹具失败", err)
		}
	}
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", stamp.Add(-time.Hour), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=? WHERE id=105", stamp.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=TIMESTAMPADD(SECOND,CASE WHEN entry_id IN (101,103) THEN 2 WHEN entry_id=113 THEN 1 ELSE 0 END,?) WHERE last_opened_at IS NOT NULL", stamp); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	query := recentEntryCandidateQuery(100)
	checkIDs := func(t *testing.T, page *domain.RecentEntryPage, expected []uint64, more bool) {
		t.Helper()
		if page == nil || page.Entries == nil || len(page.Entries) != len(expected) || page.HasMore != more {
			t.Fatal("最近访问候选页形状或续页状态错误")
		}
		for index, candidate := range page.Entries {
			if candidate.Entry.ID != expected[index] {
				t.Fatal("最近访问范围或访问时间排序错误", candidate.Entry.ID, expected[index])
			}
		}
	}

	// 2. 核对访问条件、类型/状态、用户范围和全部返回字段。
	for _, testCase := range []struct {
		name   string
		userID uint64
		ids    []uint64
	}{
		{name: "visited files only", userID: 1, ids: []uint64{103, 101, 113, 105}},
		{name: "foreign user", userID: 3, ids: []uint64{121}},
		{name: "empty user", userID: 5},
		{name: "missing user", userID: 999},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			page, err := store.ListUserRecentEntryCandidates(ctx, testCase.userID, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkIDs(t, page, testCase.ids, false)
			for _, candidate := range page.Entries {
				entry, err := store.FindUserFileEntry(ctx, testCase.userID, candidate.Entry.ID)
				if err != nil || entry == nil || !reflect.DeepEqual(candidate.Entry, *entry) {
					t.Fatal("最近访问文件基础字段与数据库不一致", err)
				}
				preference, err := store.FindUserEntryPreference(ctx, testCase.userID, candidate.Entry.ID)
				if err != nil || preference == nil || preference.LastOpenedAt == nil || !candidate.LastOpenedAt.Equal(*preference.LastOpenedAt) {
					t.Fatal("最近访问时间与当前用户偏好不一致", err)
				}
			}
		})
	}
	t.Run("pagination and offset position", func(t *testing.T) {
		filters := recentEntryCandidateQuery(2)
		var before *domain.RecentEntryPosition
		for index, ids := range [][]uint64{{103, 101}, {113, 105}, {}} {
			page, err := store.ListUserRecentEntryCandidates(ctx, 1, filters, before)
			if err != nil {
				t.Fatal(err)
			}
			checkIDs(t, page, ids, index == 0)
			if len(page.Entries) > 0 {
				last := page.Entries[len(page.Entries)-1]
				before = &domain.RecentEntryPosition{ID: last.Entry.ID, LastOpenedAt: last.LastOpenedAt.In(time.FixedZone("fixture", 8*3600))}
			}
		}
		first, err := store.ListUserRecentEntryCandidates(ctx, 1, recentEntryCandidateQuery(1), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, first, []uint64{103}, true)
		next, err := store.ListUserRecentEntryCandidates(ctx, 1, recentEntryCandidateQuery(1), &domain.RecentEntryPosition{ID: 103, LastOpenedAt: stamp.Add(2 * time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, next, []uint64{101}, true)
	})
	t.Run("same snapshot visibility with one connection", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		err := store.WithFileEntrySnapshot(ctx, func(current context.Context, reader *Store) error {
			page, err := reader.ListUserRecentEntryCandidates(current, 1, query, nil)
			if err != nil {
				return err
			}
			entries := make([]domain.FileEntry, 0, len(page.Entries))
			for _, candidate := range page.Entries {
				entries = append(entries, candidate.Entry)
			}
			visible, err := service.FilterVisibleFileEntries(current, domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}, entries, reader)
			if err != nil {
				return err
			}
			if len(visible) != 3 || visible[0].ID != 103 || visible[1].ID != 101 || visible[2].ID != 105 {
				return fmt.Errorf("最近候选与完整父链过滤衔接错误")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	// 3. 核对未提交访问写入和已提交变化的快照边界。
	t.Run("uncommitted access changes", func(t *testing.T) {
		writer, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		if _, err := writer.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=NULL WHERE user_id=1 AND entry_id=103"); err != nil {
			t.Fatal(err)
		}
		page, err := store.ListUserRecentEntryCandidates(ctx, 1, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, page, []uint64{103, 101, 113, 105}, false)
	})
	t.Run("access changes preserve existing snapshot", func(t *testing.T) {
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=IF(entry_id=103,?,NULL) WHERE user_id=1 AND entry_id IN (103,104)", stamp.Add(2*time.Second)); err != nil {
				t.Error("恢复最近访问夹具失败", err)
			}
		}()
		err := store.WithFileEntrySnapshot(ctx, func(current context.Context, reader *Store) error {
			first, err := reader.ListUserRecentEntryCandidates(current, 1, query, nil)
			if err != nil {
				return err
			}
			checkIDs(t, first, []uint64{103, 101, 113, 105}, false)
			if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=IF(entry_id=103,NULL,?) WHERE user_id=1 AND entry_id IN (103,104)", stamp.Add(3*time.Second)); err != nil {
				return err
			}
			again, err := reader.ListUserRecentEntryCandidates(current, 1, query, nil)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(first, again) {
				return fmt.Errorf("已有快照中的最近访问记录发生变化")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		current, err := store.ListUserRecentEntryCandidates(ctx, 1, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, current, []uint64{104, 101, 113, 105}, false)
	})
	t.Run("independent copies and read only preference", func(t *testing.T) {
		var createdBefore, updatedBefore time.Time
		if err := pool.QueryRowContext(ctx, "SELECT created_at,updated_at FROM user_entry_preferences WHERE user_id=1 AND entry_id=101").Scan(&createdBefore, &updatedBefore); err != nil {
			t.Fatal(err)
		}
		preferenceBefore, err := store.FindUserEntryPreference(ctx, 1, 101)
		if err != nil {
			t.Fatal(err)
		}
		page, err := store.ListUserRecentEntryCandidates(ctx, 1, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		page.Entries[0].Entry.Name = "本地修改"
		*page.Entries[0].Entry.ParentID = 999
		page.Entries[0].LastOpenedAt = stamp
		again, err := store.ListUserRecentEntryCandidates(ctx, 1, query, nil)
		if err != nil || again == nil || len(again.Entries) != 4 || again.Entries[0].Entry.Name != "访问计划.md" || *again.Entries[0].Entry.ParentID != 14 || !again.Entries[0].LastOpenedAt.Equal(stamp.Add(2*time.Second)) {
			t.Fatal("修改候选副本影响后续读取", err)
		}
		preferenceAfter, err := store.FindUserEntryPreference(ctx, 1, 101)
		if err != nil || !reflect.DeepEqual(preferenceBefore, preferenceAfter) {
			t.Fatal("最近访问读取修改偏好事实", err)
		}
		var createdAfter, updatedAfter time.Time
		if err := pool.QueryRowContext(ctx, "SELECT created_at,updated_at FROM user_entry_preferences WHERE user_id=1 AND entry_id=101").Scan(&createdAfter, &updatedAfter); err != nil || !createdBefore.Equal(createdAfter) || !updatedBefore.Equal(updatedAfter) {
			t.Fatal("最近访问读取修改偏好审计时间", err)
		}
	})
	t.Run("maximum page after access filtering", func(t *testing.T) {
		writer, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Rollback()
		for index := 0; index < 102; index++ {
			name := fmt.Sprintf("访问批次-%d", index)
			if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id) VALUES(?,5,55,?,?,'FILE','OTHER',45)", 1000+index, name, []byte(name)); err != nil {
				t.Fatal(err)
			}
			if index < 101 {
				if _, err := writer.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,last_opened_at) VALUES(5,?,?)", 1000+index, stamp); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := writer.Commit(); err != nil {
			t.Fatal(err)
		}
		page, err := store.ListUserRecentEntryCandidates(ctx, 5, query, nil)
		if err != nil || page == nil || len(page.Entries) != 100 || !page.HasMore || page.Entries[0].Entry.ID != 1100 || page.Entries[99].Entry.ID != 1001 {
			t.Fatal("百条访问候选及额外记录错误", err)
		}
		last, err := store.ListUserRecentEntryCandidates(ctx, 5, query, &domain.RecentEntryPosition{ID: 1001, LastOpenedAt: stamp})
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, last, []uint64{1000}, false)
	})

	// 4. 在普通夹具完成后验证完整无符号整数与数据库日期两端。
	t.Run("maximum identifiers and database dates", func(t *testing.T) {
		maximum := ^uint64(0)
		minimumTime := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
		maximumTime := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','最大用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','fixture-max',?)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum-1, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision,created_at,modified_at,updated_at) VALUES(?,?,?,'最大文件',CAST('最大文件' AS BINARY),'FILE','OTHER',?,?,?,?,?,?)", maximum, maximum, maximum-1, maximum, maximum, maximum, minimumTime, maximumTime, maximumTime); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,last_opened_at) VALUES(?,?,?)", maximum, maximum, minimumTime); err != nil {
			t.Fatal(err)
		}
		page, err := store.ListUserRecentEntryCandidates(ctx, maximum, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, page, []uint64{maximum}, false)
		candidate := page.Entries[0]
		if candidate.Entry.UserID != maximum || *candidate.Entry.ParentID != maximum-1 || candidate.Entry.Revision != maximum || candidate.Entry.FileSize != maximum || !candidate.Entry.CreatedAt.Equal(minimumTime) || !candidate.Entry.ModifiedAt.Equal(maximumTime) || !candidate.LastOpenedAt.Equal(minimumTime) {
			t.Fatal("完整整数、访问及修改时间边界错误")
		}
		if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=? WHERE user_id=? AND entry_id=?", maximumTime, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		upper, err := store.ListUserRecentEntryCandidates(ctx, maximum, query, nil)
		if err != nil || upper == nil || len(upper.Entries) != 1 || !upper.Entries[0].LastOpenedAt.Equal(maximumTime) {
			t.Fatal("最高访问时间精度错误", err)
		}
		last, err := store.ListUserRecentEntryCandidates(ctx, maximum, query, &domain.RecentEntryPosition{ID: maximum, LastOpenedAt: maximumTime})
		if err != nil {
			t.Fatal(err)
		}
		checkIDs(t, last, nil, false)
	})
}
