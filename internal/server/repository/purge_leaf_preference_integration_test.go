package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestClearPurgeLeafPreferenceIntegration(t *testing.T) {
	// 1. 准备清理子树的叶子和范围外的偏好记录。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, _ := trashEntryTestDatabase(t, ctx)
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备清理偏好夹具失败", err)
		}
	}
	preferencesStart := strings.Index(migrations.InitialSQL, "CREATE TABLE user_entry_preferences (")
	preferencesEnd := strings.Index(migrations.InitialSQL, "CREATE TABLE upload_tasks (")
	if preferencesStart < 0 || preferencesEnd <= preferencesStart {
		t.Fatal("目录项偏好表迁移未找到")
	}
	execute(migrations.InitialSQL[preferencesStart:preferencesEnd])
	execute("UPDATE file_entries SET status='PURGING',purge_started_at=?,revision=revision+1 WHERE id=14", stamp)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		VALUES (17,1,14,'收藏目录',CAST('收藏目录' AS BINARY),'DIR'),
		       (18,1,14,'未设置偏好的目录',CAST('未设置偏好的目录' AS BINARY),'DIR'),
		       (22,2,21,'另一账号收藏',CAST('另一账号收藏' AS BINARY),'DIR')`)
	execute(`INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at)
		VALUES (1,15,?,?), (1,17,?,NULL), (1,13,?,?), (2,22,?,NULL)`,
		stamp, stamp, stamp, stamp, stamp, stamp)

	roots, err := store.ListPurgeRootCandidates(ctx, 1)
	if err != nil || len(roots) != 1 {
		t.Fatal("未取得偏好夹具的清理根项", roots, err)
	}
	rollbackFixture := errors.New("验证偏好删除后回滚")
	err = store.WithPurgeRootTransaction(ctx, roots[0], func(txContext context.Context, txStore *Store) error {
		// 2. 锁定叶子后清除其偏好，缺失及重复清除都应成功。
		candidates, err := txStore.ListPurgeLeafCandidates(txContext, 10)
		if err != nil {
			return err
		}
		if len(candidates) != 3 || candidates[0].EntryID != 15 || candidates[1].EntryID != 17 || candidates[2].EntryID != 18 {
			return fmt.Errorf("偏好夹具的叶子候选不符：%+v", candidates)
		}
		for _, candidate := range candidates {
			leaf, err := txStore.LockPurgeLeaf(txContext, candidate)
			if err != nil {
				return err
			}
			wrongOwner := *leaf
			wrongOwner.UserID = 2
			if err := txStore.clearPurgeLeafPreference(txContext, &wrongOwner); !errors.Is(err, domain.ErrInvalidFileEntryData) {
				return fmt.Errorf("偏好清除接受了错误归属：%v", err)
			}
			for range 2 {
				if err := txStore.clearPurgeLeafPreference(txContext, leaf); err != nil {
					return err
				}
			}
		}

		// 3. 核对实际删除的行与范围外的保留行。
		for _, preference := range []struct {
			userID, entryID uint64
			want            int
		}{
			{1, 15, 0}, {1, 17, 0}, {1, 18, 0}, {1, 13, 1}, {2, 22, 1},
		} {
			var rows int
			err := txStore.database.WithContext(txContext).Raw(
				"SELECT COUNT(*) FROM user_entry_preferences WHERE user_id=? AND entry_id=?",
				preference.userID, preference.entryID).Row().Scan(&rows)
			if err != nil || rows != preference.want {
				return fmt.Errorf("用户 %d 的目录项 %d 偏好行数为 %d，期望 %d：%v",
					preference.userID, preference.entryID, rows, preference.want, err)
			}
		}
		return rollbackFixture
	})
	if !errors.Is(err, rollbackFixture) {
		t.Fatal("未完成偏好清除验证或未回滚", err)
	}

	// 4. 回查收藏和最近访问，确认回滚恢复原记录。
	preference, err := store.FindUserEntryPreference(ctx, 1, 15)
	if err != nil || preference == nil || preference.StarredAt == nil || !preference.StarredAt.Equal(stamp) ||
		preference.LastOpenedAt == nil || !preference.LastOpenedAt.Equal(stamp) {
		t.Fatal("文件的收藏和访问记录未回滚", preference, err)
	}
	preference, err = store.FindUserEntryPreference(ctx, 1, 17)
	if err != nil || preference == nil || preference.StarredAt == nil || !preference.StarredAt.Equal(stamp) {
		t.Fatal("目录收藏未回滚", preference, err)
	}
}
