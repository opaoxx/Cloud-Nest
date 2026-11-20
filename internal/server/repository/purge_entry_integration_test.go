package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestBeginUserEntryPurgeIntegration(t *testing.T) {
	// 1. 准备回收项、极限版本、内容引用和目录项偏好。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	preferencesStart := strings.Index(migrations.InitialSQL, "CREATE TABLE user_entry_preferences (")
	preferencesEnd := strings.Index(migrations.InitialSQL, "CREATE TABLE upload_tasks (")
	if preferencesStart < 0 || preferencesEnd <= preferencesStart {
		t.Fatal("目录项偏好表迁移未找到")
	}
	const deletedAtText = "2000-01-01 00:00:00.123456"
	for _, statement := range []string{
		migrations.InitialSQL[preferencesStart:preferencesEnd],
		"UPDATE users SET used_storage=4096 WHERE id=1",
		"UPDATE file_entries SET status='TRASHED', deleted_at='" + deletedAtText + "' WHERE user_id=1 AND id IN (13,16)",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) VALUES(1,13,'2000-01-01 01:02:03.123456')",
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at,purged_at,revision)
		  VALUES(18,1,11,'最后一次递增',CAST('最后一次递增' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6),NULL,NULL,18446744073709551614),
		        (24,1,11,'清理中',CAST('清理中' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),NULL,1),
		        (25,1,11,'已清理',CAST('已清理' AS BINARY),'DIR','PURGED',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),1)`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备永久清理夹具失败", err)
		}
	}
	read := func(t *testing.T, entryID uint64) *domain.FileEntry {
		t.Helper()
		entry, err := store.FindUserFileEntry(ctx, actor.UserID, entryID)
		if err != nil {
			t.Fatal(err)
		}
		return entry
	}
	begin := func(entryID, revision uint64) (*domain.FileEntry, error) {
		var saved *domain.FileEntry
		if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			var writeError error
			saved, writeError = txStore.BeginUserEntryPurge(txContext, entryID, revision)
			return writeError
		}); err != nil {
			return nil, err
		}
		return saved, nil
	}

	// 2. 拒绝不匹配的归属、状态和版本，以及缺少认证的事务。
	for _, scenario := range []struct {
		name              string
		entryID, revision uint64
		want              error
	}{
		{"true root", 11, 1, domain.ErrRootEntryPurge},
		{"foreign entry", 21, 1, domain.ErrFileEntryNotFound},
		{"missing entry", 99, 1, domain.ErrFileEntryNotFound},
		{"active entry", 12, 1, domain.ErrFileEntryNotFound},
		{"purging entry", 24, 1, domain.ErrFileEntryNotFound},
		{"purged entry", 25, 1, domain.ErrFileEntryNotFound},
		{"version conflict", 13, 2, domain.ErrEntryRevisionConflict},
		{"exhausted version", 16, math.MaxUint64, domain.ErrEntryRevisionExhausted},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			entry, err := begin(scenario.entryID, scenario.revision)
			if entry != nil || !errors.Is(err, scenario.want) {
				t.Fatal("永久清理写入应被拒绝", entry, err)
			}
			if scenario.want == domain.ErrEntryRevisionConflict {
				var conflict *domain.EntryRevisionConflictError
				if !errors.As(err, &conflict) || conflict.CurrentRevision != 3 {
					t.Fatal("版本冲突没有返回持锁读取的版本", err)
				}
			}
		})
	}
	t.Run("requires authenticated transaction", func(t *testing.T) {
		if result, err := store.BeginUserEntryPurge(ctx, 13, 3); result != nil || err == nil {
			t.Fatal("事务外调用应被拒绝", result, err)
		}
		err := store.WithLoginUserTransaction(ctx, actor.UserID, func(txContext context.Context, txStore *Store) error {
			if result, err := txStore.BeginUserEntryPurge(txContext, 13, 3); result != nil || err == nil {
				t.Fatal("缺少认证会话的事务应被拒绝", result, err)
			}
			return nil
		})
		if err != nil || read(t, 13).Status != "TRASHED" {
			t.Fatal("拒绝的调用改变了回收状态", err)
		}
	})

	// 3. 接入真实预检、首次受理和用户事务。
	masterKey := make([]byte, sha256.Size)
	signer, err := service.NewPurgePreviewSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewPurgePreviewVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Date(2026, 11, 20, 4, 30, 0, 123456000, time.UTC)
	checkClock := func() time.Time { return issuedAt.Add(time.Minute) }
	previewService := service.NewPurgePreviewService(func(ctx context.Context, action func(context.Context, service.PurgePreviewReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
			return action(snapshotContext, snapshotStore)
		})
	}, signer, func() time.Time { return issuedAt })
	purgeService := service.NewPurgeEntryService(func(ctx context.Context, identity domain.Actor, action func(context.Context, service.PurgeEntryTransaction) error) error {
		return store.WithUserTransaction(ctx, identity, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	}, verifier, checkClock)

	t.Run("file accepted after preview recheck", func(t *testing.T) {
		preview, err := previewService.Preview(ctx, actor, 13)
		if err != nil || preview == nil {
			t.Fatal("未取得可复查的文件凭证", err)
		}
		before := read(t, 13)
		var beforeWrite, afterWrite time.Time
		if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&beforeWrite); err != nil {
			t.Fatal(err)
		}
		if result, err := purgeService.Begin(ctx, actor, preview.EntryID, preview.Revision+1, preview.Credential.PreviewDigest); result != nil ||
			!errors.Is(err, domain.ErrEntryRevisionConflict) || !reflect.DeepEqual(read(t, 13), before) {
			t.Fatal("范围匹配时的版本冲突应保留原记录", result, err)
		}
		saved, err := purgeService.Begin(ctx, actor, preview.EntryID, preview.Revision, preview.Credential.PreviewDigest)
		if err != nil || saved == nil || saved.Status != "PURGING" || saved.Revision != 4 ||
			saved.ParentID == nil || *saved.ParentID != *before.ParentID || saved.Name != before.Name ||
			!saved.CreatedAt.Equal(before.CreatedAt) || !reflect.DeepEqual(saved, read(t, 13)) {
			t.Fatal("提交后的文件清理状态、版本或基本数据不符", saved, err)
		}
		if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&afterWrite); err != nil {
			t.Fatal(err)
		}
		var objectID, usedStorage uint64
		var startedAt, deletedAt, starredAt time.Time
		var purgedAt sql.NullTime
		err = pool.QueryRowContext(ctx, `SELECT e.object_id,u.used_storage,e.purge_started_at,e.deleted_at,e.purged_at,p.starred_at
		  FROM file_entries e JOIN users u ON u.id=e.user_id
		  JOIN user_entry_preferences p ON p.user_id=e.user_id AND p.entry_id=e.id WHERE e.user_id=1 AND e.id=13`).
			Scan(&objectID, &usedStorage, &startedAt, &deletedAt, &purgedAt, &starredAt)
		if err != nil || objectID != 41 || usedStorage != 4096 || purgedAt.Valid ||
			deletedAt.Format("2006-01-02 15:04:05.000000") != deletedAtText ||
			starredAt.Format("2006-01-02 15:04:05.000000") != "2000-01-01 01:02:03.123456" ||
			startedAt.Before(beforeWrite) || startedAt.After(afterWrite) || startedAt.Location() != time.UTC ||
			startedAt.Nanosecond()%1000 != 0 || !startedAt.Equal(saved.ModifiedAt) || !startedAt.Equal(saved.UpdatedAt) {
			t.Fatal("清理开始时间、回收时间、引用、用量或偏好不符", err)
		}
	})

	// 4. 子树变化应拒绝受理，写入后的事务失败应回滚。
	t.Run("stale range rollback and subtree visibility", func(t *testing.T) {
		before := read(t, 14)
		original, err := previewService.Preview(ctx, actor, before.ID)
		if err != nil || original == nil {
			t.Fatal("未取得原子树凭证", err)
		}
		err = store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			return txStore.database.WithContext(txContext).Exec("UPDATE file_entries SET revision=revision+1 WHERE user_id=1 AND id=15").Error
		})
		if err != nil {
			t.Fatal(err)
		}
		childBefore := read(t, 15)
		if result, err := purgeService.Begin(ctx, actor, before.ID, original.Revision, original.Credential.PreviewDigest); result != nil ||
			!errors.Is(err, domain.ErrPurgePreviewChanged) || !reflect.DeepEqual(read(t, 14), before) {
			t.Fatal("目录目标版本相同也应拒绝变化的子树", result, err)
		}
		current, err := previewService.Preview(ctx, actor, before.ID)
		if err != nil || current == nil || current.Revision != original.Revision {
			t.Fatal("新凭证应对应相同的目标版本", err)
		}
		rollback := errors.New("回滚永久清理夹具")
		rollbackService := service.NewPurgeEntryService(func(ctx context.Context, identity domain.Actor, action func(context.Context, service.PurgeEntryTransaction) error) error {
			return store.WithUserTransaction(ctx, identity, func(txContext context.Context, txStore *Store) error {
				if err := action(txContext, txStore); err != nil {
					return err
				}
				inside, err := txStore.FindUserFileEntry(txContext, identity.UserID, before.ID)
				if err != nil || inside == nil || inside.Status != "PURGING" {
					t.Fatal("回滚前未写入清理开始状态", inside, err)
				}
				return rollback
			})
		}, verifier, checkClock)
		result, err := rollbackService.Begin(ctx, actor, before.ID, current.Revision, current.Credential.PreviewDigest)
		var startedAt sql.NullTime
		if queryError := pool.QueryRowContext(ctx, "SELECT purge_started_at FROM file_entries WHERE user_id=1 AND id=14").Scan(&startedAt); queryError != nil ||
			result != nil || !errors.Is(err, rollback) || startedAt.Valid || !reflect.DeepEqual(read(t, 14), before) {
			t.Fatal("外层回滚没有恢复状态、版本和时间", err, queryError)
		}
		if result, err := purgeService.Begin(ctx, actor, before.ID, current.Revision, current.Credential.PreviewDigest); err != nil || result == nil || result.Status != "PURGING" ||
			result.Revision != 2 || !reflect.DeepEqual(read(t, 15), childBefore) {
			t.Fatal("目录清理受理或后代记录不符", result, err)
		}
		paths := service.NewFileEntryPathService(func(ctx context.Context, action func(context.Context, service.FileEntryPathReader) error) error {
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
				return action(snapshotContext, snapshotStore)
			})
		})
		if result, err := paths.Get(ctx, actor, 15); result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("已开始清理的目录仍允许访问后代", result, err)
		}
	})

	// 5. 并发相同版本只能受理一次，最后可递增版本完整落库。
	t.Run("concurrent requests at maximum revision", func(t *testing.T) {
		pool.SetMaxOpenConns(2)
		defer pool.SetMaxOpenConns(1)
		start := make(chan struct{})
		outcomes := make(chan error, 2)
		for range 2 {
			go func() {
				<-start
				_, err := begin(18, math.MaxUint64-1)
				outcomes <- err
			}()
		}
		close(start)
		successes, rejected := 0, 0
		for range 2 {
			select {
			case err := <-outcomes:
				switch {
				case err == nil:
					successes++
				case errors.Is(err, domain.ErrFileEntryNotFound):
					rejected++
				default:
					t.Fatal("同版本并发受理结果异常", err)
				}
			case <-ctx.Done():
				t.Fatal("等待并发受理超时", ctx.Err())
			}
		}
		entry := read(t, 18)
		if successes != 1 || rejected != 1 || entry.Status != "PURGING" || entry.Revision != math.MaxUint64 {
			t.Fatal("同版本并发受理次数或最大版本不符", successes, rejected, entry)
		}
	})
}
