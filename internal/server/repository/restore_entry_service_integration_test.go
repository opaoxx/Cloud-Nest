package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

type invalidRestoreResultStore struct {
	*Store
}

func (store invalidRestoreResultStore) RestoreUserFileEntry(ctx context.Context, entryID, revision, parentID uint64, name string, key []byte) (*domain.FileEntry, error) {
	result, err := store.Store.RestoreUserFileEntry(ctx, entryID, revision, parentID, name, key)
	if err == nil {
		result.Revision++
	}
	return result, err
}

func TestRestoreEntryServiceIntegration(t *testing.T) {
	// 1. 准备原目录、根目录、同名项和清理中的父链。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	for _, statement := range []string{
		"UPDATE file_entries SET status='TRASHED', deleted_at='2000-01-01 00:00:00.000000' WHERE user_id=1 AND id IN (13,15)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(17,1,11,'隐藏.MD',CAST('隐藏.md' AS BINARY),'DIR')",
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at)
		  VALUES(18,1,11,'清理父目录',CAST('清理父目录' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,revision)
		  VALUES(22,1,18,'回收父目录',CAST('回收父目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6),1),
		        (23,1,22,'待清理后代',CAST('待清理后代' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6),2)`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备恢复核心服务夹具失败", err)
		}
	}
	core := service.NewRestoreEntryService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RestoreEntryTransaction) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	})
	read := func(t *testing.T, entryID uint64) *domain.FileEntry {
		t.Helper()
		entry, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
		if err != nil {
			t.Fatal("读取真实目录项失败", err)
		}
		return entry
	}

	// 2. 尚未写入时拒绝不合条件的目标，清理优先于陈旧版本。
	for _, scenario := range []struct {
		entryID, revision uint64
		wantError         error
	}{
		{11, 1, domain.ErrRootEntryRestore},
		{21, 1, domain.ErrFileEntryNotFound},
		{23, 1, domain.ErrFileEntryNotFound},
		{13, 2, domain.ErrEntryRevisionConflict},
	} {
		if result, err := core.Restore(ctx, actor, scenario.entryID, scenario.revision); result != nil || !errors.Is(err, scenario.wantError) {
			t.Fatalf("目标 %d 的结果为 %+v，错误为 %v，期望 %v", scenario.entryID, result, err, scenario.wantError)
		}
	}

	// 3. 真正落库后模拟异常读回，验证服务返回错误会回滚写入。
	t.Run("bad write result rolls back", func(t *testing.T) {
		before := read(t, 13)
		broken := service.NewRestoreEntryService(func(ctx context.Context, actor domain.Actor, action func(context.Context, service.RestoreEntryTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				return action(txContext, invalidRestoreResultStore{Store: txStore})
			})
		})
		result, err := broken.Restore(ctx, actor, 13, 3)
		var deletedAt sql.NullTime
		readError := pool.QueryRowContext(ctx, "SELECT deleted_at FROM file_entries WHERE id=13").Scan(&deletedAt)
		if result != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) || readError != nil || !deletedAt.Valid ||
			!reflect.DeepEqual(read(t, 13), before) {
			t.Fatal("异常读回后没有回滚真实恢复写入", result, err, readError)
		}
	})

	// 4. 核对首次成功结果与已提交记录，再用同版本独立请求验证活动项拒绝。
	for _, scenario := range []struct {
		name                            string
		entryID, revision, wantParentID uint64
		wantName                        string
	}{
		{"original location and name", 13, 3, 12, "有效.md"},
		{"root fallback and numbered name", 15, 5, 11, "隐藏 (1).md"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			result, err := core.Restore(ctx, actor, scenario.entryID, scenario.revision)
			if err != nil || result == nil || result.Status != "ACTIVE" || result.Revision != scenario.revision+1 ||
				result.ParentID == nil || *result.ParentID != scenario.wantParentID || result.Name != scenario.wantName ||
				!reflect.DeepEqual(read(t, scenario.entryID), result) {
				t.Fatalf("服务交付的恢复结果与已提交记录不符，结果 %+v，错误 %v", result, err)
			}

			// 5. 把实际提交的目录项转换为稳定的六字段公开快照。
			snapshot, err := dto.NewRestoreEntryData(result)
			if err != nil {
				t.Fatal("无法转换恢复结果", err)
			}
			body, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal("无法编码恢复快照", err)
			}
			wantEntry := map[string]string{
				"id": strconv.FormatUint(scenario.entryID, 10), "parent_id": strconv.FormatUint(scenario.wantParentID, 10),
				"name": scenario.wantName, "entry_type": "FILE", "status": "ACTIVE",
				"revision": strconv.FormatUint(scenario.revision+1, 10),
			}
			var decoded map[string]map[string]string
			if err := json.Unmarshal(body, &decoded); err != nil || len(decoded) != 1 || !reflect.DeepEqual(decoded["entry"], wantEntry) {
				t.Fatal("提交结果的公开快照不符", string(body), err)
			}

			if again, err := core.Restore(ctx, actor, scenario.entryID, scenario.revision); again != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
				t.Fatal("再次恢复活动项时应返回未找到", again, err)
			}
		})
	}
}
