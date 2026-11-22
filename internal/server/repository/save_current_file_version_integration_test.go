package repository

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSaveCurrentFileVersionIntegration(t *testing.T) {
	// 1. 复用正式迁移中的历史表和合成文件与对象。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, _ := uploadTaskDatabase(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	const userID, entryID, oldObjectID, revision = 7, 31, 29, 6
	const retentionDays = domain.DefaultFileVersionRetentionDays
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal("准备历史引用夹具失败", err)
		}
	}
	execute(t, "UPDATE file_entries SET revision=? WHERE id=?", revision, entryID)
	execute(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size)
		VALUES (32,8,14,'foreign.bin',CAST('foreign.bin' AS BINARY),'FILE','OTHER',24,0)`)
	execute(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
		VALUES (33,7,13,'hidden',CAST('hidden' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))`)
	execute(t, "UPDATE users SET used_storage=16777216,reserved_storage=512 WHERE id=7")
	save := func(target, expected uint64, days int) (*domain.FileVersion, error) {
		var version *domain.FileVersion
		err := store.withActiveUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			var err error
			version, err = txStore.SaveCurrentFileVersion(txContext, target, expected, days)
			return err
		})
		return version, err
	}
	checkCount := func(t *testing.T, want int) {
		t.Helper()
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_versions").Scan(&count); err != nil || count != want {
			t.Fatal("历史行数不符", count, err)
		}
	}

	// 2. 在写入前拒绝无效上下文、跨账号、隐藏路径、过期版本和无效对象。
	if version, err := store.SaveCurrentFileVersion(ctx, entryID, revision, retentionDays); version != nil || err == nil {
		t.Fatal("事务外调用未被拒绝", version, err)
	}
	if version, err := save(entryID, revision, 0); version != nil || err == nil {
		t.Fatal("零保留天数未被拒绝", version, err)
	}
	for _, sample := range []struct {
		name            string
		entry, revision uint64
		days            int
		want            error
	}{
		{"foreign file", 32, 1, retentionDays, domain.ErrFileEntryNotFound},
		{"directory", 13, 1, retentionDays, domain.ErrFileEntryNotFound},
		{"wrong revision", entryID, revision - 1, retentionDays, domain.ErrEntryRevisionConflict},
		{"timestamp overflow", entryID, revision, math.MaxInt, domain.ErrInvalidFileVersionData},
	} {
		t.Run(sample.name, func(t *testing.T) {
			version, err := save(sample.entry, sample.revision, sample.days)
			if version != nil || !errors.Is(err, sample.want) {
				t.Fatal("非法历史写入结果不符", version, err)
			}
			if errors.Is(err, domain.ErrEntryRevisionConflict) {
				var conflict *domain.EntryRevisionConflictError
				if !errors.As(err, &conflict) || conflict.CurrentRevision != revision {
					t.Fatal("版本冲突没有保留当前版本", err)
				}
			}
			checkCount(t, 0)
		})
	}
	for _, sample := range []struct {
		name, change, restore string
		expected              uint64
		want                  error
	}{
		{"hidden ancestor", "UPDATE file_entries SET parent_id=33 WHERE id=31", "UPDATE file_entries SET parent_id=13 WHERE id=31", revision, domain.ErrFileEntryNotFound},
		{"maximum revision", "UPDATE file_entries SET revision=18446744073709551615 WHERE id=31", "UPDATE file_entries SET revision=6 WHERE id=31", math.MaxUint64, domain.ErrEntryRevisionExhausted},
		{"unready object", "UPDATE stored_objects SET status='VERIFYING' WHERE id=29", "UPDATE stored_objects SET status='READY' WHERE id=29", revision, domain.ErrInvalidStoredObjectData},
		{"different byte count", "UPDATE file_entries SET file_size=1 WHERE id=31", "UPDATE file_entries SET file_size=16777216 WHERE id=31", revision, domain.ErrInvalidStoredObjectData},
		{"object already timed", "UPDATE stored_objects SET unreferenced_at=UTC_TIMESTAMP(6),gc_after=UTC_TIMESTAMP(6)+INTERVAL 7 DAY WHERE id=29",
			"UPDATE stored_objects SET unreferenced_at=NULL,gc_after=NULL WHERE id=29", revision, domain.ErrInvalidStoredObjectData},
	} {
		t.Run(sample.name, func(t *testing.T) {
			execute(t, sample.change)
			t.Cleanup(func() { execute(t, sample.restore) })
			if version, err := save(entryID, sample.expected, retentionDays); version != nil || !errors.Is(err, sample.want) {
				t.Fatal("无效当前文件或对象仍生成历史", version, err)
			}
			checkCount(t, 0)
		})
	}

	// 3. 外层事务回滚时，已读回的历史行也须消失。
	rollbackFixture := errors.New("fixture stops after saving file version")
	err = store.withActiveUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
		version, err := txStore.SaveCurrentFileVersion(txContext, entryID, revision, retentionDays)
		if err != nil {
			return err
		}
		if version == nil || version.ID == 0 {
			t.Fatal("回滚前未取得历史行")
		}
		return rollbackFixture
	})
	if !errors.Is(err, rollbackFixture) {
		t.Fatal("历史写入后的外层故障未保留", err)
	}
	checkCount(t, 0)
	execute(t, "CREATE TRIGGER fixture_alter_history_size BEFORE INSERT ON file_versions FOR EACH ROW SET NEW.file_size=NEW.file_size+1")
	if version, err := save(entryID, revision, retentionDays); version != nil || !errors.Is(err, domain.ErrInvalidFileVersionData) {
		t.Fatal("写回不一致的历史行未拒绝", version, err)
	}
	execute(t, "DROP TRIGGER fixture_alter_history_size")
	checkCount(t, 0)

	// 4. 成功时保存实际旧引用，重复键不得刷新期限或改变当前文件、对象和账目。
	var before, after time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&before); err != nil {
		t.Fatal(err)
	}
	saved, err := save(entryID, revision, retentionDays)
	if err != nil || saved == nil || saved.ID == 0 || saved.UserID != userID || saved.EntryID != entryID ||
		saved.Revision != revision || saved.ObjectID != oldObjectID || saved.FileSize != 16777216 ||
		saved.CreatedAt.Before(before) || !saved.RetainedUntil.Equal(saved.CreatedAt.AddDate(0, 0, retentionDays)) {
		t.Fatal("历史行与原文件或保留期不符", saved, err)
	}
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&after); err != nil || saved.CreatedAt.After(after) {
		t.Fatal("历史行创建时间超出数据库时钟范围", err)
	}
	if version, err := save(entryID, revision, retentionDays+1); version != nil || !errors.Is(err, domain.ErrFileVersionConflict) {
		t.Fatal("重复版本未返回历史冲突", version, err)
	}
	checkCount(t, 1)
	var persistedID uint64
	var persistedDeadline time.Time
	if err := pool.QueryRowContext(ctx, "SELECT id,retained_until FROM file_versions WHERE user_id=7 AND entry_id=31 AND revision=6").
		Scan(&persistedID, &persistedDeadline); err != nil || persistedID != saved.ID || !persistedDeadline.Equal(saved.RetainedUntil) {
		t.Fatal("重复版本改动了原行", err)
	}
	var sameFacts bool
	if err := pool.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM file_entries AS entry JOIN stored_objects AS object ON object.id=entry.object_id AND object.owner_user_id=entry.user_id
		JOIN users AS account ON account.id=entry.user_id WHERE entry.id=31 AND entry.user_id=7 AND entry.revision=6
			AND entry.status='ACTIVE' AND entry.file_size=16777216 AND object.id=29 AND object.status='READY'
			AND object.unreferenced_at IS NULL AND object.gc_after IS NULL AND account.used_storage=16777216
			AND account.reserved_storage=512 AND account.change_seq=0)`).Scan(&sameFacts); err != nil || !sameFacts {
		t.Fatal("单条历史写入改动了当前引用、对象或账目", err)
	}
	t.Log("保存文件 31 的版本 6 和对象 29，数据库 UTC 起算 30 天。外层故障与异常读回均回滚，重复保留原截止时间")

	// 5. 用零字节文件和不同保留天数验证账号锁与唯一键。
	execute(t, `INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,verified_sha256,status,verified_at)
		VALUES (36,7,'MINIO','fixture-bucket','empty-history',0,UNHEX(SHA2('',256)),'READY',UTC_TIMESTAMP(6))`)
	execute(t, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision)
		VALUES (37,7,13,'empty.bin',CAST('empty.bin' AS BINARY),'FILE','OTHER',36,0,2)`)
	type attempt struct {
		version *domain.FileVersion
		err     error
	}
	start := make(chan struct{})
	results := make(chan attempt, 2)
	for range 2 {
		go func() {
			<-start
			version, err := save(37, 2, 1)
			results <- attempt{version, err}
		}()
	}
	close(start)
	var success, conflicts int
	for range 2 {
		result := <-results
		switch {
		case result.err == nil && result.version != nil:
			success++
			version := result.version
			if version.FileSize != 0 || version.ObjectID != 36 || !version.RetainedUntil.Equal(version.CreatedAt.AddDate(0, 0, 1)) {
				t.Error("零字节记录或自定义保留期不符", version)
			}
		case result.version == nil && errors.Is(result.err, domain.ErrFileVersionConflict):
			conflicts++
		default:
			t.Error("并发历史写入返回意外结果", result)
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatal("并发写入应只有一条成功", success, conflicts)
	}
	checkCount(t, 2)
	t.Log("零字节文件使用一天保留期，并发写入一项成功、一项返回重复冲突")
}
