package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

type recentEntryListSnapshotReader struct {
	*Store
	calls     *int
	afterRead func() error
}

func (reader *recentEntryListSnapshotReader) ListUserRecentEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, before *domain.RecentEntryPosition) (*domain.RecentEntryPage, error) {
	(*reader.calls)++
	page, err := reader.Store.ListUserRecentEntryCandidates(ctx, userID, query, before)
	if err == nil && reader.afterRead != nil {
		err = reader.afterRead()
	}
	return page, err
}

// verifyRecentEntryList 复用正式候选夹具，装配真实快照和最近使用可见列表。
func verifyRecentEntryList(t *testing.T, ctx context.Context, pool *sql.DB, store *Store, stamp time.Time) {
	t.Helper()
	// 1. 把当前快照 Store 的候选及祖先读取交给正式服务。
	makeList := func(afterRead func() error) (*service.RecentEntryListService, *int) {
		calls := 0
		listing := service.NewRecentEntryListService(func(current context.Context, action func(context.Context, service.RecentEntryListReader) error) error {
			return store.WithFileEntrySnapshot(current, func(snapshotContext context.Context, reader *Store) error {
				return action(snapshotContext, &recentEntryListSnapshotReader{Store: reader, calls: &calls, afterRead: afterRead})
			})
		})
		return listing, &calls
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	checkPage := func(t *testing.T, result *domain.RecentEntryList, ids []uint64, more bool, nextID uint64) {
		t.Helper()
		if result == nil || result.Entries == nil || len(result.Entries) != len(ids) || result.HasMore != more || (result.NextPosition != nil) != more {
			t.Fatal("真实最近可见列表形状或续页提示错误")
		}
		for index, candidate := range result.Entries {
			position := domain.RecentEntryPosition{ID: candidate.Entry.ID, LastOpenedAt: candidate.LastOpenedAt}
			if candidate.Entry.ID != ids[index] || candidate.Entry.EntryType != "FILE" || candidate.Entry.Status != "ACTIVE" || !position.IsValid() {
				t.Fatal("真实最近可见文件或访问时间错误")
			}
		}
		if more && (result.NextPosition.ID != nextID || !result.NextPosition.IsValid() || result.NextPosition.LastOpenedAt.Location() != time.UTC) {
			t.Fatal("真实最近列表最后扫描位置错误")
		}
	}
	t.Run("visible pages and access continuation", func(t *testing.T) {
		listing, _ := makeList(nil)
		query := recentEntryCandidateQuery(1)
		var before *domain.RecentEntryPosition
		for index, ids := range [][]uint64{{103}, {101}, {105}, {}} {
			result, err := listing.List(ctx, actor, query, before)
			if err != nil {
				t.Fatal(err)
			}
			var nextID uint64
			if index < 2 {
				nextID = ids[0]
			}
			checkPage(t, result, ids, index < 2, nextID)
			if len(result.Entries) > 0 {
				last := result.Entries[0]
				before = &domain.RecentEntryPosition{ID: last.Entry.ID, LastOpenedAt: last.LastOpenedAt.In(time.FixedZone("fixture", 8*3600))}
				if last.Entry.ID == 103 && !last.LastOpenedAt.Equal(stamp.Add(2*time.Second)) {
					t.Fatal("最近列表丢失原访问时间")
				}
			}
		}
	})
	t.Run("search kind ownership and account status", func(t *testing.T) {
		listing, _ := makeList(nil)
		for _, fixture := range []struct {
			search, kind string
			ids          []uint64
		}{
			{"访问", "document", []uint64{103}},
			{"访问", "image", []uint64{105}},
			{"学习", "document", []uint64{101}},
			{"隐藏", "document", nil},
			{"", "folder", nil},
		} {
			query := recentEntryCandidateQuery(50)
			query.Search, query.Kind = fixture.search, fixture.kind
			result, err := listing.List(ctx, actor, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkPage(t, result, fixture.ids, false, 0)
		}
		other := actor
		other.UserID = 3
		foreign, err := listing.List(ctx, other, recentEntryCandidateQuery(2), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, foreign, []uint64{121}, false, 0)
		other.UserID = 999
		if result, err := listing.List(ctx, other, recentEntryCandidateQuery(2), nil); result != nil || !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatal("空候选跳过用户状态检查", err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=5"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=5"); err != nil {
				t.Error("恢复账号状态失败", err)
			}
		}()
		other.UserID = 5
		if result, err := listing.List(ctx, other, recentEntryCandidateQuery(2), nil); result != nil || !errors.Is(err, domain.ErrAccountDisabled) {
			t.Fatal("空最近列表跳过禁用账号检查", err)
		}
	})
	t.Run("single connection", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		listing, _ := makeList(nil)
		result, err := listing.List(ctx, actor, recentEntryCandidateQuery(10), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, result, []uint64{103, 101, 105}, false, 0)
	})
	// 2. 在候选读取后提交外部变化，确认访问与祖先检查使用原快照。
	t.Run("parent move after candidate read", func(t *testing.T) {
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=14 WHERE id=103"); err != nil {
				t.Error("恢复最近文件父目录失败", err)
			}
		}()
		listing, _ := makeList(func() error {
			_, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=16 WHERE id=103")
			return err
		})
		result, err := listing.List(ctx, actor, recentEntryCandidateQuery(10), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, result, []uint64{103, 101, 105}, false, 0)
		current, _ := makeList(nil)
		again, err := current.List(ctx, actor, recentEntryCandidateQuery(10), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, again, []uint64{101, 105}, false, 0)
	})
	t.Run("access changes after candidate read", func(t *testing.T) {
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=IF(entry_id=103,?,NULL) WHERE user_id=1 AND entry_id IN (103,104)", stamp.Add(2*time.Second)); err != nil {
				t.Error("恢复最近文件访问偏好失败", err)
			}
		}()
		listing, _ := makeList(func() error {
			_, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=IF(entry_id=103,NULL,?) WHERE user_id=1 AND entry_id IN (103,104)", stamp.Add(5*time.Second))
			return err
		})
		result, err := listing.List(ctx, actor, recentEntryCandidateQuery(10), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, result, []uint64{103, 101, 105}, false, 0)
		current, _ := makeList(nil)
		again, err := current.List(ctx, actor, recentEntryCandidateQuery(10), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, again, []uint64{104, 101, 105}, false, 0)
	})

	// 3. 在独占库准备一千零一个隐藏候选及更早的两个可见文件。
	defer func() {
		for _, statement := range []string{
			"DELETE FROM user_entry_preferences WHERE entry_id IN (2901,2903) OR entry_id BETWEEN 3000 AND 4000",
			"DELETE FROM file_entries WHERE id IN (2901,2903) OR id BETWEEN 3000 AND 4000",
		} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Error("清理最近列表预算夹具失败", err)
			}
		}
	}()
	writer, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	for index := 0; index < 1003; index++ {
		entryID, parentID := 3000+index, 16
		name := fmt.Sprintf("最近预算-隐藏-%d.md", index)
		openedAt := stamp.Add(3 * time.Second)
		if index >= 1001 {
			entryID, parentID = 2901+(index-1001)*2, 12
			name = fmt.Sprintf("最近预算-可见-%d.md", entryID)
			openedAt = stamp.Add(-time.Second)
		}
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,?,?,?,'FILE','DOCUMENT',41,2048)", entryID, parentID, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,last_opened_at) VALUES(1,?,?)", entryID, openedAt); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	budgetQuery := recentEntryCandidateQuery(2)
	budgetQuery.Search, budgetQuery.Kind = "最近预算-", "document"
	t.Run("empty budget continues to visible files", func(t *testing.T) {
		listing, calls := makeList(nil)
		first, err := listing.List(ctx, actor, budgetQuery, nil)
		if err != nil || *calls != 10 {
			t.Fatal("最近预算扫描批次错误", err)
		}
		checkPage(t, first, nil, true, 3001)
		if !first.NextPosition.LastOpenedAt.Equal(stamp.Add(3 * time.Second)) {
			t.Fatal("隐藏末项访问时间丢失")
		}
		next, err := listing.List(ctx, actor, budgetQuery, first.NextPosition)
		if err != nil || *calls != 11 {
			t.Fatal("最近预算空页续读错误", err)
		}
		checkPage(t, next, []uint64{2903, 2901}, false, 0)
	})
	t.Run("short page uses hidden last position", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=12 WHERE id=4000"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=16 WHERE id=4000"); err != nil {
				t.Error(err)
			}
		}()
		listing, _ := makeList(nil)
		first, err := listing.List(ctx, actor, budgetQuery, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, first, []uint64{4000}, true, 3001)
		next, err := listing.List(ctx, actor, budgetQuery, first.NextPosition)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, next, []uint64{2903, 2901}, false, 0)
	})
	t.Run("full page at budget edge", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=12 WHERE id=3001"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=16 WHERE id=3001"); err != nil {
				t.Error(err)
			}
		}()
		listing, calls := makeList(nil)
		query := budgetQuery
		query.PageSize = 1
		first, err := listing.List(ctx, actor, query, nil)
		if err != nil || *calls != 10 {
			t.Fatal("预算边界满页错误", err)
		}
		checkPage(t, first, []uint64{3001}, true, 3001)
		next, err := listing.List(ctx, actor, query, first.NextPosition)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, next, []uint64{2903}, true, 2903)
	})
	t.Run("exact budget end", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=3000"); err != nil {
			t.Fatal(err)
		}
		listing, calls := makeList(nil)
		query := budgetQuery
		query.Search = "最近预算-隐藏-"
		result, err := listing.List(ctx, actor, query, nil)
		if err != nil || *calls != 10 {
			t.Fatal("恰好最近预算结束错误", err)
		}
		checkPage(t, result, nil, false, 0)
	})
}
