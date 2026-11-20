package repository

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestListUserTrashEntryCandidatesIntegration(t *testing.T) {
	// 1. 在独占验证库插入同一微秒的回收项、嵌套项和其他状态及用户的记录。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	stamp := time.Date(2026, 11, 20, 1, 30, 0, 123456000, time.UTC)
	execute := func(statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal("准备回收站候选夹具失败", err)
		}
	}
	execute(`INSERT INTO users(id,username,display_name,password_hash)
		VALUES (1,'fixture_owner','回收用户','fixture_hash'),
		       (3,'fixture_other','其他用户','fixture_hash')`)
	execute(`INSERT INTO file_entries(id,user_id,name,name_key,entry_type)
		VALUES (11,1,'',X'','DIR'), (33,3,'',X'','DIR')`)
	execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size)
		VALUES (41,1,'MINIO','fixture-bucket','fixture-trash',2048)`)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
		VALUES (12,1,11,'回收父目录',CAST('回收父目录' AS BINARY),'DIR','TRASHED',?),
		       (13,1,11,'同刻目录',CAST('同刻目录' AS BINARY),'DIR','TRASHED',?),
		       (99,1,11,'较早目录',CAST('较早目录' AS BINARY),'DIR','TRASHED',?),
		       (31,3,33,'他人目录',CAST('他人目录' AS BINARY),'DIR','TRASHED',?),
		       (16,1,11,'活动目录',CAST('活动目录' AS BINARY),'DIR','ACTIVE',NULL)`,
		stamp.Add(time.Microsecond), stamp, stamp.Add(-time.Microsecond), stamp.Add(time.Second))
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,category,object_id,file_size,revision)
		VALUES (14,1,12,'嵌套文件',CAST('嵌套文件' AS BINARY),'FILE','TRASHED',?,'OTHER',41,2048,3),
		       (15,1,11,'同刻文件',CAST('同刻文件' AS BINARY),'FILE','TRASHED',?,'OTHER',41,2048,2)`, stamp, stamp)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at,purged_at)
		VALUES (17,1,11,'清理中',CAST('清理中' AS BINARY),'DIR','PURGING',?,?,NULL),
		       (18,1,11,'已清理',CAST('已清理' AS BINARY),'DIR','PURGED',?,?,?)`, stamp, stamp, stamp, stamp, stamp)

	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	checkBatch := func(t *testing.T, readContext context.Context, reader *Store, userID uint64, limit int, before *domain.TrashEntryPosition, ids []uint64, more bool) []domain.TrashedFileEntry {
		t.Helper()
		page, err := reader.ListUserTrashEntryCandidates(readContext, userID, limit, before)
		if err != nil || page == nil || page.Entries == nil {
			t.Fatalf("读取回收候选失败，页为 %+v，错误为 %v", page, err)
		}
		actualIDs := make([]uint64, 0, len(page.Entries))
		for _, entry := range page.Entries {
			if entry.UserID != userID || entry.Status != "TRASHED" || entry.DeletedAt.Location() != time.UTC {
				t.Fatalf("候选归属、状态或回收时间不是预期值，候选为 %+v", entry)
			}
			actualIDs = append(actualIDs, entry.ID)
		}
		if !slices.Equal(actualIDs, ids) || page.HasMore != more {
			t.Fatalf("候选为 %v，HasMore 为 %v，期望 %v 和 %v", actualIDs, page.HasMore, ids, more)
		}
		return page.Entries
	}

	// 2. 验证直接读取的用户边界及回收状态。
	t.Run("user and state", func(t *testing.T) {
		entries := checkBatch(t, ctx, store, 1, 100, nil, []uint64{12, 15, 14, 13, 99}, false)
		nested := entries[2]
		if nested.ParentID == nil || *nested.ParentID != 12 || nested.Name != "嵌套文件" ||
			nested.EntryType != "FILE" || nested.FileSize != 2048 || nested.Revision != 3 || !nested.DeletedAt.Equal(stamp) {
			t.Fatalf("嵌套回收候选的字段错误，候选为 %+v", nested)
		}
		checkBatch(t, ctx, store, 3, 100, nil, []uint64{31}, false)
		checkBatch(t, ctx, store, 9, 100, nil, nil, false)
	})

	// 3. 在只读快照中用上一批末项续读，确认同一回收时间的记录不丢失。
	t.Run("exclusive pagination in snapshot", func(t *testing.T) {
		err := store.WithFileEntrySnapshot(ctx, func(readContext context.Context, reader *Store) error {
			var before *domain.TrashEntryPosition
			offset := time.FixedZone("UTC+08", 8*60*60)
			for index, ids := range [][]uint64{{12, 15}, {14, 13}, {99}, nil} {
				batch := checkBatch(t, readContext, reader, 1, 2, before, ids, index < 2)
				if len(batch) != 0 {
					last := batch[len(batch)-1]
					before = &domain.TrashEntryPosition{ID: last.ID, DeletedAt: last.DeletedAt.In(offset)}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	// 4. 模拟父目录恢复并移位，验证旧快照和新读取各自的祖先状态与路径。
	t.Run("ancestor visibility in snapshot", func(t *testing.T) {
		actor := domain.Actor{UserID: 1, SessionID: 17, DeviceID: 19}
		readNestedPath := func(readContext context.Context, reader *Store) (*domain.FileEntryPath, error) {
			page, err := reader.ListUserTrashEntryCandidates(readContext, actor.UserID, 100, nil)
			if err != nil {
				return nil, err
			}
			for _, candidate := range page.Entries {
				if candidate.ID == 14 {
					return service.CheckTrashEntryVisibility(readContext, actor, candidate, reader)
				}
			}
			return nil, fmt.Errorf("未读取到嵌套回收候选")
		}
		err := store.WithFileEntrySnapshot(ctx, func(readContext context.Context, reader *Store) error {
			before, err := readNestedPath(readContext, reader)
			if err != nil || before != nil {
				return fmt.Errorf("父目录被回收时，原父路径为 %+v，错误为 %v", before, err)
			}
			_, err = pool.ExecContext(ctx, `UPDATE file_entries
				SET status='ACTIVE', deleted_at=NULL, parent_id=16,
				    name='恢复后改名', name_key=CAST('恢复后改名' AS BINARY)
				WHERE user_id=1 AND id=12`)
			if err != nil {
				return err
			}
			again, err := readNestedPath(readContext, reader)
			if err != nil || again != nil {
				return fmt.Errorf("旧快照中的原父路径为 %+v，错误为 %v", again, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		err = store.WithFileEntrySnapshot(ctx, func(readContext context.Context, reader *Store) error {
			path, err := readNestedPath(readContext, reader)
			if err != nil || path == nil || len(path.Entries) != 3 || path.Entries[0].ID != 11 ||
				path.Entries[1].ID != 16 || path.Entries[2].ID != 12 || path.Entries[2].Name != "恢复后改名" {
				return fmt.Errorf("新快照中的原父路径为 %+v，错误为 %v", path, err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	// 5. 在独占库中用真实的千条隐藏候选验证空页续读。
	t.Run("list continues after empty budget page", func(t *testing.T) {
		_, err := pool.ExecContext(ctx, `INSERT INTO file_entries
			(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
			VALUES (44,3,33,'可见目录一',CAST('可见目录一' AS BINARY),'DIR','TRASHED',?),
			       (45,3,33,'可见目录二',CAST('可见目录二' AS BINARY),'DIR','TRASHED',?)`, stamp, stamp)
		if err != nil {
			t.Fatal(err)
		}
		hiddenTime := stamp.Add(time.Minute)
		_, err = pool.ExecContext(ctx, `INSERT INTO file_entries
			(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
			WITH RECURSIVE fixture_ids AS (
				SELECT 0 AS fixture_index
				UNION ALL SELECT fixture_index+1 FROM fixture_ids WHERE fixture_index < 999
			)
			SELECT 2000+fixture_index, 3, 31, CONCAT('隐藏目录-', fixture_index),
			       CAST(CONCAT('隐藏目录-', fixture_index) AS BINARY), 'DIR', 'TRASHED', ?
			FROM fixture_ids`, hiddenTime)
		if err != nil {
			t.Fatal(err)
		}
		list := service.NewTrashEntryListService(func(listContext context.Context, read func(context.Context, service.TrashEntryListReader) error) error {
			return store.WithFileEntrySnapshot(listContext, func(snapshotContext context.Context, reader *Store) error {
				return read(snapshotContext, reader)
			})
		})
		actor := domain.Actor{UserID: 3, SessionID: 17, DeviceID: 19}
		var before *domain.TrashEntryPosition
		for index, expected := range [][]uint64{{}, {31, 45}, {44}} {
			page, err := list.List(ctx, actor, 2, before)
			if err != nil || page == nil || page.Entries == nil {
				t.Fatalf("第 %d 页为 %+v，错误为 %v", index+1, page, err)
			}
			ids := make([]uint64, 0, len(page.Entries))
			for _, item := range page.Entries {
				ids = append(ids, item.Entry.ID)
				path := item.OriginalPath.Entries
				if len(path) == 0 || path[0].ID != 33 || path[len(path)-1].ID != *item.Entry.ParentID {
					t.Fatalf("目录项 %d 的原父路径为 %+v", item.Entry.ID, path)
				}
			}
			if !slices.Equal(ids, expected) || (page.NextPosition != nil) != (index < 2) {
				t.Fatalf("第 %d 页项目为 %v，续读位置为 %+v", index+1, ids, page.NextPosition)
			}
			if index == 0 && (page.NextPosition.ID != 2000 || !page.NextPosition.DeletedAt.Equal(hiddenTime)) {
				t.Fatalf("空页续读点为 %+v，期望最后消费的隐藏候选 2000", page.NextPosition)
			}
			if index == 0 {
				// 用本页返回的位置签发游标，再从客户端查询参数读回原文。
				signer, err := service.NewTrashEntryCursorSigner(bytes.Repeat([]byte{9}, 32))
				if err != nil {
					t.Fatal(err)
				}
				cursor, err := signer.Sign(actor.UserID, *page.NextPosition)
				if err != nil {
					t.Fatal(err)
				}
				query, fieldError := dto.ParseTrashPageQuery(url.Values{"cursor": {cursor}, "limit": {"2"}}.Encode())
				if fieldError != nil || query == nil || query.Cursor != cursor || query.PageSize != 2 {
					t.Fatalf("空页游标无法经 URL 查询原样传递，字段错误为 %v", fieldError)
				}
				payload, _, _ := strings.Cut(query.Cursor, ".")
				decoded, err := base64.RawURLEncoding.Strict().DecodeString(payload)
				want := `{"version":1,"user_id":"3","deleted_at":"2026-11-20T01:31:00.123456Z","entry_id":"2000"}`
				if err != nil || string(decoded) != want {
					t.Fatalf("空页游标载荷为 %s，错误为 %v", decoded, err)
				}
			}
			before = page.NextPosition
		}
	})
}
