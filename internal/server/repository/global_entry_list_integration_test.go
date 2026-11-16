package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

type globalEntryListSnapshotReader struct {
	*Store
	calls     *int
	afterRead func() error
}

func (reader *globalEntryListSnapshotReader) ListUserGlobalEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, before *domain.FileEntryPosition) (*domain.FileEntryPage, error) {
	(*reader.calls)++
	page, err := reader.Store.ListUserGlobalEntryCandidates(ctx, userID, query, before)
	if err == nil && reader.afterRead != nil {
		err = reader.afterRead()
	}
	return page, err
}

func TestGlobalEntryListServiceIntegration(t *testing.T) {
	testGlobalEntryListServiceIntegration(t, domain.GlobalEntryScopeAll)
}

func TestStarredGlobalEntryListServiceIntegration(t *testing.T) {
	testGlobalEntryListServiceIntegration(t, domain.GlobalEntryScopeStarred)
}

func testGlobalEntryListServiceIntegration(t *testing.T, scope string) {
	t.Helper()
	// 1. 准备正式表候选及真实快照服务装配。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','全局分页用户','fixture_hash','ACTIVE'),(3,'fixture_other','其他用户','fixture_hash','ACTIVE'),(5,'fixture_disabled','禁用用户','fixture_hash','DISABLED')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-global-list-object',2048),(43,3,'MINIO','fixture-bucket','fixture-other-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(55,5,NULL,'',X'','DIR'),(12,1,11,'文档目录',CAST('文档目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(14,1,11,'回收目录',CAST('回收目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(13,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048),(15,1,12,'学习计划.md',CAST('学习计划.md' AS BINARY),'FILE','DOCUMENT',41,2048),(16,1,14,'隐藏学习.md',CAST('隐藏学习.md' AS BINARY),'FILE','DOCUMENT',41,2048),(43,3,33,'其他账号.md',CAST('其他账号.md' AS BINARY),'FILE','DOCUMENT',43,2048)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立全局分页夹具失败", err)
		}
	}
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=? WHERE id IN (13,15)", stamp.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=? WHERE id=16", stamp.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if scope == domain.GlobalEntryScopeStarred {
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
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) SELECT user_id,id,? FROM file_entries", stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,created_at,modified_at,updated_at) VALUES(18,1,12,'学习未收藏.md',CAST('学习未收藏.md' AS BINARY),'FILE','DOCUMENT',41,2048,?,?,?)", stamp, stamp.Add(time.Second), stamp); err != nil {
			t.Fatal(err)
		}
	}
	queryForPage := func(size int) domain.GlobalEntryQuery {
		query := globalEntryCandidateQuery(size)
		query.Scope = scope
		return query
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	makeList := func(afterRead func() error) (*service.GlobalEntryListService, *int) {
		calls := 0
		listing := service.NewGlobalEntryListService(func(current context.Context, action func(context.Context, service.GlobalEntryListReader) error) error {
			return store.WithFileEntrySnapshot(current, func(txContext context.Context, txStore *Store) error {
				return action(txContext, &globalEntryListSnapshotReader{Store: txStore, calls: &calls, afterRead: afterRead})
			})
		})
		return listing, &calls
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	cursorSigner, err := service.NewGlobalEntryCursorSigner(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	cursorVerifier, err := service.NewGlobalEntryCursorVerifier(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	restoreNextPosition := func(t *testing.T, query domain.GlobalEntryQuery, position *domain.FileEntryPosition) *domain.FileEntryPosition {
		t.Helper()
		// 1. 将实际最后消费位置签名，使用调整后的合法页大小校验。
		if position == nil {
			t.Fatal("真实续页缺少最后消费位置")
		}
		original := *position
		cursor, err := cursorSigner.Sign(actor.UserID, query, original)
		if err != nil {
			t.Fatal(err)
		}
		resized := query
		resized.PageSize = 100
		restored, err := cursorVerifier.Verify(actor.UserID, resized, cursor)
		if err != nil || restored == nil || !reflect.DeepEqual(*restored, original) || restored == position || *position != original {
			t.Fatal("真实列表游标签名、还原或独立结果错误", err)
		}
		// 2. 同一密钥下，两类查询范围的游标绑定准确。
		other := query
		other.Scope = domain.GlobalEntryScopeAll
		if query.Scope == domain.GlobalEntryScopeAll {
			other.Scope = domain.GlobalEntryScopeStarred
		}
		if invalid, err := cursorVerifier.Verify(actor.UserID, other, cursor); invalid != nil || !errors.Is(err, domain.ErrInvalidGlobalEntryCursor) {
			t.Fatal("不同查询范围通过真实列表游标校验", err)
		}
		return restored
	}
	checkPage := func(t *testing.T, result *domain.GlobalEntryList, ids []uint64, more bool, nextID uint64) {
		t.Helper()
		if result == nil || result.Entries == nil || len(result.Entries) != len(ids) || result.HasMore != more || (result.NextPosition != nil) != more {
			t.Fatal("可见列表长度、空切片或后续提示错误")
		}
		if more && (result.NextPosition.ID != nextID || !result.NextPosition.IsValid() || result.NextPosition.ModifiedAt.Location() != time.UTC) {
			t.Fatal("最后消费位置错误")
		}
		for index, entryID := range ids {
			if result.Entries[index].ID != entryID {
				t.Fatal("可见项顺序错误")
			}
		}
	}

	checkTextPage := func(t *testing.T, query domain.GlobalEntryQuery, result *domain.GlobalEntryPageResult, ids []uint64, nextID uint64) {
		t.Helper()
		if result == nil || result.Entries == nil || len(result.Entries) != len(ids) || (result.NextCursor != nil) != (nextID != 0) {
			t.Fatal("真实文本分页结果或续页形状错误")
		}
		for index, entry := range result.Entries {
			stored, err := store.FindUserFileEntry(ctx, actor.UserID, entry.ID)
			if entry.ID != ids[index] || err != nil || stored == nil || !reflect.DeepEqual(entry, *stored) {
				t.Fatal("文本分页基础字段或顺序错误", err)
			}
		}
		if result.NextCursor != nil {
			position, err := cursorVerifier.Verify(actor.UserID, query, *result.NextCursor)
			if err != nil || position == nil || position.ID != nextID {
				t.Fatal("文本分页游标绑定或最后消费位置错误", err)
			}
			stored, err := store.FindUserFileEntry(ctx, actor.UserID, nextID)
			if err != nil || stored == nil || !position.ModifiedAt.Equal(stored.ModifiedAt) {
				t.Fatal("文本分页游标修改时间错误", err)
			}
		}
		// 实际服务结果继续转换为公开数据，检查空数组和明确结束值。
		data, err := dto.NewGlobalEntryPageData(result)
		if err != nil || data == nil || data.Items == nil || data.Breadcrumbs == nil || len(data.Items) != len(ids) || len(data.Breadcrumbs) != 0 {
			t.Fatal("真实文本分页响应 DTO 错误", err)
		}
		encoded, err := json.Marshal(data)
		var fields map[string]json.RawMessage
		if err != nil || json.Unmarshal(encoded, &fields) != nil || len(fields) != 3 || string(fields["breadcrumbs"]) != "[]" || (len(ids) == 0 && string(fields["items"]) != "[]") || (nextID == 0 && string(fields["next_cursor"]) != "null") {
			t.Fatal("文本分页 JSON 公开字段或空值错误", err)
		}
		if result.NextCursor != nil && (data.NextCursor == nil || *data.NextCursor != *result.NextCursor || data.NextCursor == result.NextCursor) {
			t.Fatal("文本分页游标转换或副本错误")
		}
	}
	t.Run("text cursor first next end and binding", func(t *testing.T) {
		// 1. 真实空游标首页与合法页大小调整后的续页。
		listing, calls := makeList(nil)
		paging := service.NewGlobalEntryPageService(cursorVerifier.Verify, listing.List, cursorSigner.Sign)
		query := queryForPage(1)
		query.Search, query.Kind = "学习", "document"
		first, err := paging.List(ctx, actor, query, "")
		if err != nil {
			t.Fatal(err)
		}
		checkTextPage(t, query, first, []uint64{15}, 15)
		query.PageSize = 2
		next, err := paging.List(ctx, actor, query, *first.NextCursor)
		if err != nil {
			t.Fatal(err)
		}
		checkTextPage(t, query, next, []uint64{13}, 0)
		// 2. 无效用户/范围/筛选或篡改的游标在候选读取前被拒绝。
		for _, mode := range []string{"user", "scope", "search", "kind", "tampered"} {
			identity, filters, cursor := actor, query, *first.NextCursor
			switch mode {
			case "user":
				identity.UserID = 3
			case "scope":
				filters.Scope = domain.GlobalEntryScopeAll
				if query.Scope == domain.GlobalEntryScopeAll {
					filters.Scope = domain.GlobalEntryScopeStarred
				}
			case "search":
				filters.Search += " "
			case "kind":
				filters.Kind = "image"
			case "tampered":
				cursor += "="
			}
			beforeCalls := *calls
			result, err := paging.List(ctx, identity, filters, cursor)
			if result != nil || !errors.Is(err, domain.ErrInvalidGlobalEntryCursor) || *calls != beforeCalls {
				t.Fatal("错误文本游标进入了候选读取", mode, err)
			}
		}
		// 3. 空结束页和空列表账号状态通过真实服务链检查。
		query.Search = "无匹配项"
		empty, err := paging.List(ctx, actor, query, "")
		if err != nil {
			t.Fatal(err)
		}
		checkTextPage(t, query, empty, nil, 0)
		disabled := actor
		disabled.UserID = 5
		if result, err := paging.List(ctx, disabled, query, ""); result != nil || !errors.Is(err, domain.ErrAccountDisabled) {
			t.Fatal("文本空列表未复查账号状态", err)
		}
	})

	// 2. 验证页内截断、续页、分类、搜索与账号范围。
	t.Run("unconsumed candidates and tuple continuation", func(t *testing.T) {
		listing, _ := makeList(nil)
		query := queryForPage(1)
		query.Kind = "document"
		first, err := listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, first, []uint64{15}, true, 15)
		second, err := listing.List(ctx, actor, query, restoreNextPosition(t, query, first.NextPosition))
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, second, []uint64{13}, false, 0)
		for _, result := range []*domain.GlobalEntryList{first, second} {
			for _, entry := range result.Entries {
				stored, err := store.FindUserFileEntry(ctx, 1, entry.ID)
				if err != nil || stored == nil || !reflect.DeepEqual(entry, *stored) {
					t.Fatal("分页基础字段与数据库事实不一致", err)
				}
			}
		}
	})
	t.Run("all types search and empty account checks", func(t *testing.T) {
		listing, _ := makeList(nil)
		query := queryForPage(2)
		first, err := listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, first, []uint64{15, 13}, true, 13)
		second, err := listing.List(ctx, actor, query, restoreNextPosition(t, query, first.NextPosition))
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, second, []uint64{12}, false, 0)
		query.Search, query.Kind = "笔记", "document"
		found, err := listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, found, []uint64{13}, false, 0)
		query.Search = "无匹配项"
		empty, err := listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, empty, nil, false, 0)
		other := actor
		other.UserID = 5
		result, err := listing.List(ctx, other, query, nil)
		if result != nil || !errors.Is(err, domain.ErrAccountDisabled) {
			t.Fatal("空列表未复查禁用账号", err)
		}
		other.UserID, query.Search = 3, ""
		foreign, err := listing.List(ctx, other, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, foreign, []uint64{43}, false, 0)
	})

	// 3. 验证单连接和候选读取后并发变更的快照。
	t.Run("single connection", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(2)
		listing, _ := makeList(nil)
		query := queryForPage(100)
		query.Kind = "document"
		result, err := listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, result, []uint64{15, 13}, false, 0)
	})
	t.Run("snapshot after candidate read", func(t *testing.T) {
		changed := false
		listing, _ := makeList(func() error {
			if changed {
				return nil
			}
			changed = true
			_, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=14 WHERE id=15")
			return err
		})
		query := queryForPage(100)
		query.Kind = "document"
		original, err := listing.List(ctx, actor, query, nil)
		if err != nil || !changed {
			t.Fatal("候选读取后并发变更失败", err)
		}
		checkPage(t, original, []uint64{15, 13}, false, 0)
		current, err := listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, current, []uint64{13}, false, 0)
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=12 WHERE id=15"); err != nil {
			t.Fatal(err)
		}
	})

	if scope == domain.GlobalEntryScopeStarred {
		t.Run("ancestors need no favorite", func(t *testing.T) {
			if _, err := pool.ExecContext(ctx, "DELETE FROM user_entry_preferences WHERE user_id=1 AND entry_id IN (12,14)"); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) VALUES(1,12,?),(1,14,?)", stamp, stamp); err != nil {
					t.Error("恢复祖先收藏夹具失败", err)
				}
			}()
			listing, _ := makeList(nil)
			query := queryForPage(100)
			query.Kind = "document"
			result, err := listing.List(ctx, actor, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkPage(t, result, []uint64{15, 13}, false, 0)
		})
		t.Run("favorite changes after candidate read", func(t *testing.T) {
			defer func() {
				if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=? WHERE user_id=1 AND entry_id=15", stamp); err != nil {
					t.Error("恢复文档收藏失败", err)
				}
				if _, err := pool.ExecContext(ctx, "DELETE FROM user_entry_preferences WHERE user_id=1 AND entry_id=18"); err != nil {
					t.Error("移除新增收藏夹具失败", err)
				}
			}()
			changed := false
			listing, _ := makeList(func() error {
				if changed {
					return nil
				}
				changed = true
				if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=NULL WHERE user_id=1 AND entry_id=15"); err != nil {
					return err
				}
				_, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) VALUES(1,18,?)", stamp)
				return err
			})
			query := queryForPage(100)
			query.Kind = "document"
			original, err := listing.List(ctx, actor, query, nil)
			if err != nil || !changed {
				t.Fatal("读取后的收藏变化未成功提交", err)
			}
			checkPage(t, original, []uint64{15, 13}, false, 0)
			current, err := listing.List(ctx, actor, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkPage(t, current, []uint64{18, 13}, false, 0)
		})
	}

	// 4. 验证千项预算的空页、短页及边界续页。
	writer, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	for index := 0; index < 1001; index++ {
		name := fmt.Sprintf("blocked-%d.md", index)
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,modified_at) VALUES(?,1,14,?,?,'FILE','DOCUMENT',41,2048,?)", 1000+index, name, []byte(name), stamp.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	if scope == domain.GlobalEntryScopeStarred {
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) SELECT user_id,id,? FROM file_entries WHERE id BETWEEN 1000 AND 2000", stamp); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("text cursor budget empty next and end", func(t *testing.T) {
		listing, calls := makeList(nil)
		paging := service.NewGlobalEntryPageService(cursorVerifier.Verify, listing.List, cursorSigner.Sign)
		query := queryForPage(1)
		query.Kind = "document"
		first, err := paging.List(ctx, actor, query, "")
		if err != nil || *calls != 10 {
			t.Fatal("文本预算空页扫描次数错误", err)
		}
		checkTextPage(t, query, first, nil, 1001)
		second, err := paging.List(ctx, actor, query, *first.NextCursor)
		if err != nil || *calls != 11 {
			t.Fatal("文本预算空页续读错误", err)
		}
		checkTextPage(t, query, second, []uint64{15}, 15)
		third, err := paging.List(ctx, actor, query, *second.NextCursor)
		if err != nil || *calls != 12 {
			t.Fatal("文本预算分页结束错误", err)
		}
		checkTextPage(t, query, third, []uint64{13}, 0)
	})
	t.Run("empty budget page continues to visible entries", func(t *testing.T) {
		listing, calls := makeList(nil)
		query := queryForPage(2)
		query.Kind = "document"
		first, err := listing.List(ctx, actor, query, nil)
		if err != nil || *calls != 10 {
			t.Fatal("预算扫描次数错误", err)
		}
		checkPage(t, first, nil, true, 1001)
		second, err := listing.List(ctx, actor, query, restoreNextPosition(t, query, first.NextPosition))
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, second, []uint64{15, 13}, false, 0)
	})
	t.Run("short page uses hidden last position", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=12 WHERE id=2000"); err != nil {
			t.Fatal(err)
		}
		listing, _ := makeList(nil)
		query := queryForPage(3)
		query.Kind = "document"
		first, err := listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, first, []uint64{2000}, true, 1001)
		second, err := listing.List(ctx, actor, query, restoreNextPosition(t, query, first.NextPosition))
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, second, []uint64{15, 13}, false, 0)
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=14 WHERE id=2000"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("page fills at last budget candidate", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=12 WHERE id=1001"); err != nil {
			t.Fatal(err)
		}
		listing, calls := makeList(nil)
		query := queryForPage(1)
		query.Kind = "document"
		first, err := listing.List(ctx, actor, query, nil)
		if err != nil || *calls != 10 {
			t.Fatal("预算边界补读次数错误", err)
		}
		checkPage(t, first, []uint64{1001}, true, 1001)
		second, err := listing.List(ctx, actor, query, restoreNextPosition(t, query, first.NextPosition))
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, second, []uint64{15}, true, 15)
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=14 WHERE id=1001"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("exact budget end has no continuation", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=1000"); err != nil {
			t.Fatal(err)
		}
		listing, calls := makeList(nil)
		query := queryForPage(2)
		query.Search, query.Kind = "blocked-", "document"
		result, err := listing.List(ctx, actor, query, nil)
		if err != nil || *calls != 10 {
			t.Fatal("恰好预算时的读取次数错误", err)
		}
		checkPage(t, result, nil, false, 0)
	})
}
