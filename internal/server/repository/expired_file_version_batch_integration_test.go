package repository

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestExpiredFileVersionBatchIntegration(t *testing.T) {
	// 1. 准备四条到期历史，第一项的配额写入会被隔离触发器拒绝。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(queryContext context.Context, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(queryContext, statement, args...); err != nil {
			t.Fatal("准备历史批次夹具失败", err)
		}
	}
	const contentSize, reservedBytes = 16777216, 19
	execute(ctx, "UPDATE file_entries SET revision=6 WHERE id=31")
	execute(ctx, "UPDATE users SET used_storage=?,reserved_storage=? WHERE id=7", 5*contentSize, reservedBytes)
	execute(ctx, `INSERT INTO file_versions(id,user_id,entry_id,revision,object_id,file_size,created_at,retained_until)
		VALUES(51,7,31,1,29,?,UTC_TIMESTAMP(6)-INTERVAL 31 DAY,UTC_TIMESTAMP(6)-INTERVAL 1 DAY),
			  (52,7,31,2,29,?,UTC_TIMESTAMP(6)-INTERVAL 31 DAY,UTC_TIMESTAMP(6)-INTERVAL 1 DAY),
			  (53,7,31,3,29,?,UTC_TIMESTAMP(6)-INTERVAL 31 DAY,UTC_TIMESTAMP(6)-INTERVAL 1 DAY),
			  (54,7,31,4,29,?,UTC_TIMESTAMP(6)-INTERVAL 31 DAY,UTC_TIMESTAMP(6)-INTERVAL 1 DAY)`,
		contentSize, contentSize, contentSize, contentSize)
	execute(ctx, `CREATE TRIGGER fixture_fail_first_version_quota BEFORE UPDATE ON users
		FOR EACH ROW BEGIN
			IF OLD.id=7 AND NEW.used_storage<OLD.used_storage
				AND NOT EXISTS(SELECT 1 FROM file_versions WHERE user_id=7 AND id=51) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture rejects first history quota update';
			END IF;
		END`)

	// 2. 只扫描一次，扫描后延后第二项截止时间以模拟候选失效。
	scans := 0
	batch := service.NewExpiredFileVersionBatch(func(scanContext context.Context, limit int) ([]domain.FileVersion, error) {
		scans++
		candidates, err := store.ListExpiredFileVersionCandidates(scanContext, limit)
		if err != nil {
			return nil, err
		}
		if len(candidates) != 3 || candidates[0].ID != 51 || candidates[1].ID != 52 || candidates[2].ID != 53 {
			t.Fatal("首批候选顺序或上限不符", candidates)
		}
		execute(scanContext, "UPDATE file_versions SET retained_until=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE user_id=7 AND id=52")
		return candidates, nil
	}, store.CompleteExpiredFileVersion)
	result, err := batch.RunOnce(ctx, 3)
	if err != nil || scans != 1 || result.Scanned != 3 || result.Settled != 1 ||
		result.StaleCandidates != 1 || len(result.CandidateErrors) != 1 {
		t.Fatal("混合批次结果不符", result, err)
	}
	failure := result.CandidateErrors[0]
	var mysqlError *mysql.MySQLError
	if failure.UserID != 7 || failure.VersionID != 51 || !errors.As(failure.Err, &mysqlError) ||
		mysqlError.Number != 1644 || mysqlError.Message != "fixture rejects first history quota update" {
		t.Fatal("第一项未保留归属、历史 ID 或原始错误", failure)
	}

	// 3. 只有第三项提交。失败项已回滚，失效项和批量外的历史仍保留。
	var remaining []uint64
	queryError := store.database.WithContext(ctx).Table("file_versions").Where("user_id = ?", 7).
		Order("id ASC").Pluck("id", &remaining).Error
	if queryError != nil || !slices.Equal(remaining, []uint64{51, 52, 54}) {
		t.Fatal("结算后的历史记录不符", remaining, queryError)
	}
	var used, reserved, sequence uint64
	if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage,change_seq FROM users WHERE id=7").
		Scan(&used, &reserved, &sequence); err != nil || used != 4*contentSize || reserved != reservedBytes || sequence != 0 {
		t.Fatal("批次只应扣减第三项的计费字节", used, reserved, sequence, err)
	}
	object, err := store.FindUserStoredObject(ctx, 7, 29)
	if err != nil || object.Status != domain.StoredObjectStatusReady || object.UnreferencedAt != nil || object.GCAfter != nil {
		t.Fatal("共享对象仍有引用，无需登记回收计时", err)
	}
	t.Logf("首轮扫描 3 条，历史 51 失败回滚，52 失效，53 成功，54 留待下轮；已用 %d B，预占 %d B", used, reserved)
}
