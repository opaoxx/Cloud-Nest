package repository

import (
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

type sizeEntryListSnapshotReader struct {
	*Store
	calls     *int
	afterRead func() error
}

func (reader *sizeEntryListSnapshotReader) ListUserSizeEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, before *domain.SizeEntryPosition) (*domain.FileEntryPage, error) {
	(*reader.calls)++
	page, err := reader.Store.ListUserSizeEntryCandidates(ctx, userID, query, before)
	if err == nil && reader.afterRead != nil {
		err = reader.afterRead()
	}
	return page, err
}

func verifySizeEntryList(t *testing.T, ctx context.Context, pool *sql.DB, store *Store) {
	t.Helper()
	// 1. 在同一快照中组合大小候选和已有祖先读取。
	makeList := func(afterRead func() error) (*service.SizeEntryListService, *int) {
		calls := 0
		listing := service.NewSizeEntryListService(func(current context.Context, action func(context.Context, service.SizeEntryListReader) error) error {
			return store.WithFileEntrySnapshot(current, func(snapshotContext context.Context, reader *Store) error {
				return action(snapshotContext, &sizeEntryListSnapshotReader{Store: reader, calls: &calls, afterRead: afterRead})
			})
		})
		return listing, &calls
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	restore := func(statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Error("恢复大小列表夹具失败", err)
		}
	}
	check := func(t *testing.T, result *domain.SizeEntryList, ids []uint64, nextID uint64) {
		t.Helper()
		if result == nil || result.Entries == nil || len(result.Entries) != len(ids) || result.HasMore != (nextID != 0) || (result.NextPosition != nil) != (nextID != 0) {
			t.Fatal("大小可见列表形状或位置错误")
		}
		for index, entry := range result.Entries {
			stored, err := store.FindUserFileEntry(ctx, entry.UserID, entry.ID)
			if err != nil || stored == nil || entry.ID != ids[index] || !reflect.DeepEqual(entry, *stored) {
				t.Fatal("大小可见项与数据库事实不符", err)
			}
		}
		if nextID != 0 {
			var size uint64
			if err := pool.QueryRowContext(ctx, "SELECT file_size FROM file_entries WHERE user_id=1 AND id=?", nextID).Scan(&size); err != nil || result.NextPosition.ID != nextID || result.NextPosition.FileSize != size || !result.NextPosition.IsValid() {
				t.Fatal("大小最后消费位置与数据库不符", err)
			}
		}
	}
	t.Run("all and starred visible pagination", func(t *testing.T) {
		for _, scope := range []string{"all", "starred"} {
			listing, _ := makeList(nil)
			query := sizeEntryCandidateQuery(2)
			query.Scope = scope
			expected := [][]uint64{{103, 101}, {105, 109}, {107, 14}, {12}}
			if scope == "starred" {
				expected = [][]uint64{{103, 101}, {107}}
			}
			var before *domain.SizeEntryPosition
			for index, ids := range expected {
				page, err := listing.List(ctx, actor, query, before)
				if err != nil {
					t.Fatal(err)
				}
				var nextID uint64
				if index+1 < len(expected) {
					nextID = ids[len(ids)-1]
				}
				check(t, page, ids, nextID)
				before = page.NextPosition
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
			{"all", "folder", "", 1, []uint64{107, 14, 12}}, {"all", "document", "readme", 1, []uint64{103, 101}},
			{"starred", "document", "", 1, []uint64{103, 101}}, {"all", "", "hidden", 1, nil},
			{"all", "", "", 3, []uint64{121}}, {"all", "", "", 5, nil},
		} {
			query := sizeEntryCandidateQuery(100)
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
		if result, err := listing.List(ctx, identity, sizeEntryCandidateQuery(2), nil); result != nil || !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatal("大小空页跳过账号检查", err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=5"); err != nil {
			t.Fatal(err)
		}
		defer restore("UPDATE users SET status='ACTIVE' WHERE id=5")
		identity.UserID = 5
		if result, err := listing.List(ctx, identity, sizeEntryCandidateQuery(2), nil); result != nil || !errors.Is(err, domain.ErrAccountDisabled) {
			t.Fatal("大小空页禁用账号被放行", err)
		}
	})
	t.Run("single connection", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		listing, _ := makeList(nil)
		result, err := listing.List(ctx, actor, sizeEntryCandidateQuery(100), nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, result, []uint64{103, 101, 105, 109, 107, 14, 12}, 0)
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
		query := sizeEntryCandidateQuery(100)
		query.Scope = "starred"
		original, err := listing.List(ctx, actor, query, nil)
		if err != nil || original == nil || len(original.Entries) != 3 || original.Entries[0].ID != 103 {
			t.Fatal("大小父链快照未保留移动前可见项", err)
		}
		current, _ := makeList(nil)
		result, err := current.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, result, []uint64{101, 107}, 0)
	})
	t.Run("size and membership snapshot", func(t *testing.T) {
		defer restore("UPDATE file_entries SET file_size=4096 WHERE id=103")
		defer restore("UPDATE user_entry_preferences SET starred_at=UTC_TIMESTAMP(6) WHERE user_id=1 AND entry_id=101")
		changed := false
		listing, _ := makeList(func() error {
			if changed {
				return nil
			}
			changed = true
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET file_size=16384 WHERE id=103"); err != nil {
				return err
			}
			_, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=NULL WHERE user_id=1 AND entry_id=101")
			return err
		})
		query := sizeEntryCandidateQuery(100)
		query.Scope = "starred"
		original, err := listing.List(ctx, actor, query, nil)
		if err != nil || original == nil || len(original.Entries) != 3 || original.Entries[0].ID != 103 || original.Entries[0].FileSize != 4096 || original.Entries[1].ID != 101 {
			t.Fatal("大小快照未保留原大小及收藏", err)
		}
		current, _ := makeList(nil)
		result, err := current.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, result, []uint64{103, 107}, 0)
	})

	// 2. 准备一千零一项隐藏候选及两项可见文件，验证扫描预算。
	writer, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	for index := 0; index < 1001; index++ {
		name := fmt.Sprintf("size-budget-hidden-%04d", index)
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,16,?,?,'FILE','DOCUMENT',41,8192)", 3000+index, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct {
		entryID, size uint64
		name          string
	}{{2901, 1024, "size-budget-visible-a"}, {2903, 512, "size-budget-visible-b"}} {
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,12,?,?,'FILE','DOCUMENT',41,?)", entry.entryID, entry.name, []byte(entry.name), entry.size); err != nil {
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
		for _, statement := range []string{
			"DELETE FROM user_entry_preferences WHERE user_id=1 AND (entry_id IN (2901,2903) OR entry_id BETWEEN 3000 AND 4000)",
			"DELETE FROM file_entries WHERE user_id=1 AND (id IN (2901,2903) OR id BETWEEN 3000 AND 4000)",
		} {
			if _, err := pool.ExecContext(cleanupContext, statement); err != nil {
				t.Error("清理大小预算夹具失败", err)
			}
		}
	}()
	for _, scope := range []string{"all", "starred"} {
		t.Run("budget empty continuation "+scope, func(t *testing.T) {
			listing, calls := makeList(nil)
			query := sizeEntryCandidateQuery(2)
			query.Scope, query.Search, query.Kind = scope, "size-budget-", "document"
			first, err := listing.List(ctx, actor, query, nil)
			if err != nil || *calls != 10 {
				t.Fatal("大小预算批次错误", err)
			}
			check(t, first, nil, 3001)
			next, err := listing.List(ctx, actor, query, first.NextPosition)
			if err != nil || *calls != 11 {
				t.Fatal("大小预算续读错误", err)
			}
			check(t, next, []uint64{2901, 2903}, 0)
		})
	}
	t.Run("short page and budget edge", func(t *testing.T) {
		defer restore("UPDATE file_entries SET parent_id=16 WHERE id IN (4000,3001)")
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=12 WHERE id=4000"); err != nil {
			t.Fatal(err)
		}
		listing, _ := makeList(nil)
		query := sizeEntryCandidateQuery(3)
		query.Search = "size-budget-"
		first, err := listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, first, []uint64{4000}, 3001)
		next, err := listing.List(ctx, actor, query, first.NextPosition)
		if err != nil {
			t.Fatal(err)
		}
		check(t, next, []uint64{2901, 2903}, 0)
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=IF(id=3001,12,16) WHERE id IN (4000,3001)"); err != nil {
			t.Fatal(err)
		}
		query.PageSize = 1
		first, err = listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, first, []uint64{3001}, 3001)
	})
	t.Run("exact budget end", func(t *testing.T) {
		defer restore("UPDATE file_entries SET status='ACTIVE',deleted_at=NULL WHERE id=3000")
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=3000"); err != nil {
			t.Fatal(err)
		}
		listing, calls := makeList(nil)
		query := sizeEntryCandidateQuery(2)
		query.Search = "size-budget-hidden-"
		result, err := listing.List(ctx, actor, query, nil)
		if err != nil || *calls != 10 {
			t.Fatal("大小恰好预算结束错误", err)
		}
		check(t, result, nil, 0)
	})
}
