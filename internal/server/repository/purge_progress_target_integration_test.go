package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestFindUserPurgeProgressTargetIntegration(t *testing.T) {
	// 1. 准备清理中、独立完成、随祖先完成以及普通目录项。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	pool.SetMaxOpenConns(2)
	startedAt := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	purgedAt := startedAt.Add(time.Second + time.Microsecond)
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries
		(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at,purged_at)
		VALUES (17,1,14,'清理中',CAST('清理中' AS BINARY),'DIR','PURGING',?,NULL),
		       (18,1,11,'独立完成',CAST('独立完成' AS BINARY),'DIR','PURGED',?,?),
		       (22,1,18,'随祖先完成',CAST('随祖先完成' AS BINARY),'DIR','PURGED',NULL,?),
		       (?,1,11,'最大标识',CAST('最大标识' AS BINARY),'DIR','PURGED',?,?)`,
		startedAt, startedAt, purgedAt, purgedAt, uint64(math.MaxUint64), startedAt, purgedAt); err != nil {
		t.Fatal("准备进度目标夹具失败", err)
	}

	// 2. 核对当前用户获得的原始状态，以及 NULL 和微秒时间的区别。
	rootID, trashedParentID, finishedParentID := uint64(11), uint64(14), uint64(18)
	for _, scenario := range []struct {
		name string
		want domain.PurgeProgressTarget
	}{
		{"real root", domain.PurgeProgressTarget{ID: 11, UserID: 1, Status: "ACTIVE"}},
		{"active directory", domain.PurgeProgressTarget{ID: 12, UserID: 1, ParentID: &rootID, Status: "ACTIVE"}},
		{"trashed directory", domain.PurgeProgressTarget{ID: 14, UserID: 1, ParentID: &rootID, Status: "TRASHED"}},
		{"purging under hidden parent", domain.PurgeProgressTarget{ID: 17, UserID: 1, ParentID: &trashedParentID, Status: "PURGING", PurgeStartedAt: &startedAt}},
		{"independently purged", domain.PurgeProgressTarget{ID: 18, UserID: 1, ParentID: &rootID, Status: "PURGED", PurgeStartedAt: &startedAt, PurgedAt: &purgedAt}},
		{"purged with ancestor", domain.PurgeProgressTarget{ID: 22, UserID: 1, ParentID: &finishedParentID, Status: "PURGED", PurgedAt: &purgedAt}},
		{"maximum uint64 ID", domain.PurgeProgressTarget{ID: math.MaxUint64, UserID: 1, ParentID: &rootID, Status: "PURGED", PurgeStartedAt: &startedAt, PurgedAt: &purgedAt}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			record, err := store.FindUserPurgeProgressTarget(ctx, actor.UserID, scenario.want.ID)
			if err != nil || !reflect.DeepEqual(record, &scenario.want) {
				t.Fatalf("进度数据不符，实际 %+v，预期 %+v，错误 %v", record, scenario.want, err)
			}
		})
	}
	for _, entryID := range []uint64{21, 99} {
		record, err := store.FindUserPurgeProgressTarget(ctx, actor.UserID, entryID)
		if record != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("跨账号和缺失目标应返回相同的未找到错误", err)
		}
	}
	for _, pair := range [][2]uint64{{0, 17}, {actor.UserID, 0}} {
		record, err := store.FindUserPurgeProgressTarget(ctx, pair[0], pair[1])
		if record != nil || err == nil || errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("零用户或零目标应返回输入错误", err)
		}
	}
	canceledContext, stop := context.WithCancel(ctx)
	stop()
	if record, err := store.FindUserPurgeProgressTarget(canceledContext, actor.UserID, 17); record != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("应保留调用方取消错误", err)
	}

	// 3. 让服务使用同一快照中的真实记录，判断不同目录项的查询资格。
	t.Run("progress target eligibility", func(t *testing.T) {
		err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
			for _, scenario := range []struct {
				entryID uint64
				status  string
				want    error
			}{
				{11, "", domain.ErrFileEntryNotFound},
				{12, "", domain.ErrFileEntryNotFound},
				{14, "", domain.ErrFileEntryNotFound},
				{17, "PURGING", nil},
				{18, "PURGED", nil},
				{21, "", domain.ErrFileEntryNotFound},
				{22, "", domain.ErrFileEntryNotFound},
				{99, "", domain.ErrFileEntryNotFound},
			} {
				target, err := service.ReadPurgeProgressTarget(snapshotContext, actor, scenario.entryID, snapshotStore)
				if !errors.Is(err, scenario.want) || (target == nil) != (scenario.want != nil) ||
					(target != nil && target.Status != scenario.status) {
					return fmt.Errorf("目标 %d 返回 %+v、错误 %v，预期状态 %q、错误 %v", scenario.entryID, target, err, scenario.status, scenario.want)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})

	// 4. 让另一个连接提交清理完成，验证旧快照和新读取各自的状态。
	t.Run("completion visible after snapshot", func(t *testing.T) {
		err := store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
			before, err := snapshotStore.FindUserPurgeProgressTarget(snapshotContext, actor.UserID, 17)
			if err != nil {
				return err
			}
			if before.Status != "PURGING" || before.PurgedAt != nil {
				return fmt.Errorf("首读应看到仍在清理的目标")
			}
			if _, err := pool.ExecContext(snapshotContext, "UPDATE file_entries SET status='PURGED',purged_at=? WHERE user_id=1 AND id=17", purgedAt); err != nil {
				return err
			}
			during, err := snapshotStore.FindUserPurgeProgressTarget(snapshotContext, actor.UserID, 17)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(during, before) {
				return fmt.Errorf("保留同一快照期间读取到了已提交的新状态")
			}
			return nil
		})
		if err != nil {
			t.Fatal("清理进度的快照读取失败", err)
		}
		after, err := store.FindUserPurgeProgressTarget(ctx, actor.UserID, 17)
		want := domain.PurgeProgressTarget{
			ID: 17, UserID: actor.UserID, ParentID: &trashedParentID, Status: "PURGED",
			PurgeStartedAt: &startedAt, PurgedAt: &purgedAt,
		}
		if err != nil || !reflect.DeepEqual(after, &want) {
			t.Fatal("新的读取应看到已完成状态和两个清理时间", after, err)
		}
	})

	// 5. 关闭测试连接，确认数据库故障仍与目标缺失区分。
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if record, err := store.FindUserPurgeProgressTarget(ctx, actor.UserID, 17); record != nil || err == nil || errors.Is(err, domain.ErrFileEntryNotFound) {
		t.Fatal("数据库故障不应当作目标不存在", err)
	}
}
