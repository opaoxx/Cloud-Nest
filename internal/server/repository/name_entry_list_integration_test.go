package repository

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

type nameEntryListSnapshotReader struct {
	*Store
	calls     *int
	afterRead func() error
}

func (reader *nameEntryListSnapshotReader) ListUserNameEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, after *domain.NameEntryPosition) (*domain.NameEntryPage, error) {
	(*reader.calls)++
	page, err := reader.Store.ListUserNameEntryCandidates(ctx, userID, query, after)
	if err == nil && reader.afterRead != nil {
		err = reader.afterRead()
	}
	return page, err
}

func verifyNameEntryList(t *testing.T, ctx context.Context, pool *sql.DB, store *Store) {
	t.Helper()
	// 1. 绑定同一快照中的名称候选与祖先读取，检查每项数据库事实。
	makeList := func(afterRead func() error) (*service.NameEntryListService, *int) {
		calls := 0
		listing := service.NewNameEntryListService(func(current context.Context, action func(context.Context, service.NameEntryListReader) error) error {
			return store.WithFileEntrySnapshot(current, func(snapshotContext context.Context, reader *Store) error {
				return action(snapshotContext, &nameEntryListSnapshotReader{Store: reader, calls: &calls, afterRead: afterRead})
			})
		})
		return listing, &calls
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	restore := func(statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Error("恢复名称列表夹具失败", err)
		}
	}
	check := func(t *testing.T, result *domain.NameEntryList, ids []uint64, nextID uint64) {
		t.Helper()
		if result == nil || result.Entries == nil || len(result.Entries) != len(ids) || result.HasMore != (nextID != 0) || (result.NextPosition != nil) != (nextID != 0) {
			t.Fatal("名称可见列表形状或位置错误")
		}
		for index, candidate := range result.Entries {
			stored, err := store.FindUserFileEntry(ctx, candidate.Entry.UserID, candidate.Entry.ID)
			var key []byte
			keyError := pool.QueryRowContext(ctx, "SELECT name_key FROM file_entries WHERE user_id=? AND id=?", candidate.Entry.UserID, candidate.Entry.ID).Scan(&key)
			if err != nil || keyError != nil || stored == nil || candidate.Entry.ID != ids[index] || !reflect.DeepEqual(candidate.Entry, *stored) || !bytes.Equal(candidate.NameKey, key) {
				t.Fatal("名称可见项或名称键与数据库不符", err, keyError)
			}
		}
		if nextID != 0 {
			var key []byte
			if err := pool.QueryRowContext(ctx, "SELECT name_key FROM file_entries WHERE user_id=1 AND id=?", nextID).Scan(&key); err != nil || result.NextPosition.ID != nextID || !result.NextPosition.IsValid() || !bytes.Equal(result.NextPosition.NameKey, key) {
				t.Fatal("最后消费名称位置与数据库不符", err)
			}
		}
	}
	t.Run("all and starred visible pagination", func(t *testing.T) {
		for _, scope := range []string{"all", "starred"} {
			listing, _ := makeList(nil)
			query := nameEntryCandidateQuery(2)
			query.Scope = scope
			expected := [][]uint64{{105, 107}, {12, 14}, {101, 103}, {109}}
			if scope == "starred" {
				expected = [][]uint64{{107, 101}, {103}}
			}
			var after *domain.NameEntryPosition
			for index, ids := range expected {
				result, err := listing.List(ctx, actor, query, after)
				if err != nil {
					t.Fatal(err)
				}
				var nextID uint64
				if index+1 < len(expected) {
					nextID = ids[len(ids)-1]
				}
				check(t, result, ids, nextID)
				after = result.NextPosition
			}
		}
	})
	t.Run("filters users and account state", func(t *testing.T) {
		listing, _ := makeList(nil)
		for _, fixture := range []struct {
			scope, kind, search string
			userID              uint64
			ids                 []uint64
		}{
			{scope: "all", userID: 1, kind: "folder", ids: []uint64{107, 12, 14}},
			{scope: "all", userID: 1, kind: "document", search: "readme", ids: []uint64{101, 103}},
			{scope: "starred", userID: 1, kind: "document", ids: []uint64{101, 103}},
			{scope: "all", userID: 1, search: "b-hidden"},
			{scope: "all", userID: 3, ids: []uint64{121}},
			{scope: "all", userID: 5},
		} {
			query := nameEntryCandidateQuery(100)
			query.Scope, query.Kind, query.Search = fixture.scope, fixture.kind, fixture.search
			identity := actor
			identity.UserID = fixture.userID
			result, err := listing.List(ctx, identity, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			check(t, result, fixture.ids, 0)
		}
		identity := actor
		identity.UserID = 999
		if result, err := listing.List(ctx, identity, nameEntryCandidateQuery(2), nil); result != nil || !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatal("空名称候选跳过账号检查", err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=5"); err != nil {
			t.Fatal(err)
		}
		defer restore("UPDATE users SET status='ACTIVE' WHERE id=5")
		identity.UserID = 5
		if result, err := listing.List(ctx, identity, nameEntryCandidateQuery(2), nil); result != nil || !errors.Is(err, domain.ErrAccountDisabled) {
			t.Fatal("空候选禁用账号被放行", err)
		}
	})
	t.Run("single connection", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		listing, _ := makeList(nil)
		result, err := listing.List(ctx, actor, nameEntryCandidateQuery(100), nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, result, []uint64{105, 107, 12, 14, 101, 103, 109}, 0)
	})
	t.Run("parent move snapshot", func(t *testing.T) {
		defer restore("UPDATE file_entries SET parent_id=11 WHERE id=14")
		changed := false
		listing, _ := makeList(func() error {
			if changed {
				return nil
			}
			changed = true
			_, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=16 WHERE id=14")
			return err
		})
		query := nameEntryCandidateQuery(100)
		query.Scope = "starred"
		result, err := listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result == nil || len(result.Entries) != 3 || result.Entries[2].Entry.ID != 103 {
			t.Fatal("原快照丢失移动前可见项")
		}
		current, _ := makeList(nil)
		result, err = current.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, result, []uint64{107, 101}, 0)
	})
	t.Run("name and membership snapshots", func(t *testing.T) {
		defer restore("UPDATE file_entries SET name='readme.md',name_key=CAST('readme.md' AS BINARY) WHERE id=103")
		defer restore("UPDATE user_entry_preferences SET starred_at=UTC_TIMESTAMP(6) WHERE user_id=1 AND entry_id=101")
		changed := false
		listing, _ := makeList(func() error {
			if changed {
				return nil
			}
			changed = true
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name='AA.md',name_key=CAST('aa.md' AS BINARY) WHERE id=103"); err != nil {
				return err
			}
			_, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=NULL WHERE user_id=1 AND entry_id=101")
			return err
		})
		query := nameEntryCandidateQuery(100)
		query.Scope = "starred"
		original, err := listing.List(ctx, actor, query, nil)
		if err != nil || original == nil || len(original.Entries) != 3 || original.Entries[1].Entry.ID != 101 || original.Entries[2].Entry.ID != 103 || string(original.Entries[2].NameKey) != "readme.md" || original.Entries[2].Entry.Name != "readme.md" {
			t.Fatal("快照未保留原名称、键或收藏成员", err)
		}
		current, _ := makeList(nil)
		result, err := current.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, result, []uint64{103, 107}, 0)
	})

	// 2. 准备一千零一项隐藏候选，验证有界补读及最后消费位置。
	writer, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	for index := 0; index < 1001; index++ {
		name := fmt.Sprintf("name-budget-hidden-%04d", index)
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,16,?,?,'FILE','DOCUMENT',41,2048)", 3000+index, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct {
		id   uint64
		name string
	}{{2901, "name-budget-visible-a"}, {2903, "name-budget-visible-b"}} {
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,12,?,?,'FILE','DOCUMENT',41,2048)", entry.id, entry.name, []byte(entry.name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) SELECT user_id,id,UTC_TIMESTAMP(6) FROM file_entries WHERE user_id=1 AND (id IN (2901,2903) OR id BETWEEN 3000 AND 4000)"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, statement := range []string{"DELETE FROM user_entry_preferences WHERE user_id=1 AND (entry_id IN (2901,2903) OR entry_id BETWEEN 3000 AND 4000)", "DELETE FROM file_entries WHERE user_id=1 AND (id IN (2901,2903) OR id BETWEEN 3000 AND 4000)"} {
			if _, err := pool.ExecContext(cleanupContext, statement); err != nil {
				t.Error("清理名称预算夹具失败", err)
			}
		}
	}()
	for _, scope := range []string{"all", "starred"} {
		t.Run("budget empty continuation "+scope, func(t *testing.T) {
			listing, calls := makeList(nil)
			query := nameEntryCandidateQuery(2)
			query.Scope, query.Search, query.Kind = scope, "name-budget-", "document"
			first, err := listing.List(ctx, actor, query, nil)
			if err != nil || *calls != 10 {
				t.Fatal("名称预算扫描批次错误", err)
			}
			check(t, first, nil, 3999)
			next, err := listing.List(ctx, actor, query, first.NextPosition)
			if err != nil || *calls != 11 {
				t.Fatal("名称预算续读错误", err)
			}
			check(t, next, []uint64{2901, 2903}, 0)
		})
	}
	t.Run("short page and budget edge", func(t *testing.T) {
		defer restore("UPDATE file_entries SET parent_id=16 WHERE id IN (3000,3999)")
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=12 WHERE id=3000"); err != nil {
			t.Fatal(err)
		}
		listing, _ := makeList(nil)
		query := nameEntryCandidateQuery(3)
		query.Search = "name-budget-"
		first, err := listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, first, []uint64{3000}, 3999)
		next, err := listing.List(ctx, actor, query, first.NextPosition)
		if err != nil {
			t.Fatal(err)
		}
		check(t, next, []uint64{2901, 2903}, 0)
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=IF(id=3999,12,16) WHERE id IN (3000,3999)"); err != nil {
			t.Fatal(err)
		}
		query.PageSize = 1
		first, err = listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, first, []uint64{3999}, 3999)
	})
	t.Run("exact budget end", func(t *testing.T) {
		defer restore("UPDATE file_entries SET status='ACTIVE',deleted_at=NULL WHERE id=4000")
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=4000"); err != nil {
			t.Fatal(err)
		}
		listing, calls := makeList(nil)
		query := nameEntryCandidateQuery(2)
		query.Search = "name-budget-hidden-"
		result, err := listing.List(ctx, actor, query, nil)
		if err != nil || *calls != 10 {
			t.Fatal("名称恰好预算结束错误", err)
		}
		check(t, result, nil, 0)
	})
}
