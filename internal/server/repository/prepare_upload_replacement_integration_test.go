package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestPrepareUploadReplacementIntegration(t *testing.T) {
	// 1. 在独占库准备真实授权记录、同账号的可见及隐藏文件和另一账号的文件。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	sessionStart := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	sessionEnd := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if sessionStart < 0 || sessionEnd <= sessionStart {
		t.Fatal("正式会话表定义未找到")
	}
	for _, statement := range []string{
		migrations.InitialSQL[sessionStart:sessionEnd],
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES (17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES (33,7,13,'parent',X'706172656E74','DIR','ACTIVE',NULL),(34,7,13,'hidden',X'68696464656E','DIR','TRASHED',UTC_TIMESTAMP(6))",
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES
			(32,8,14,'other.bin',CAST('other.bin' AS BINARY),'FILE','OTHER',24,0,9),
			(35,7,34,'hidden.bin',CAST('hidden.bin' AS BINARY),'FILE','OTHER',29,16777216,9),
			(36,7,33,'maximum.bin',CAST('maximum.bin' AS BINARY),'FILE','OTHER',29,16777216,18446744073709551615)`,
		"UPDATE file_entries SET parent_id=33,revision=6 WHERE id=31",
		"UPDATE users SET used_storage=16777216,reserved_storage=512 WHERE id=7",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备替换夹具失败", err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	const singleLimit = domain.MaxSingleUploadSizeBytes
	const storageLimit = 2 * singleLimit
	limits := domain.UploadPlanLimits{MaxFileSizeBytes: domain.DefaultMaxUploadSizeBytes, MaxObjectSizeBytes: storageLimit, MaxPartSizeBytes: singleLimit}
	read := func(t *testing.T, declaration service.UploadReplacementDeclaration, checkLocks bool) (*service.UploadReplacementPreparation, error) {
		t.Helper()
		var prepared *service.UploadReplacementPreparation
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			var err error
			prepared, err = service.PrepareUploadReplacement(txContext, actor, declaration, limits, txStore)
			if err != nil || !checkLocks {
				return err
			}
			for _, lockedID := range []uint64{31, 33, 13} {
				var id uint64
				var lockError *mysql.MySQLError
				err := pool.QueryRowContext(txContext, "SELECT id FROM file_entries WHERE id=? FOR UPDATE NOWAIT", lockedID).Scan(&id)
				if !errors.As(err, &lockError) || lockError.Number != 3572 {
					t.Fatalf("只读准备返回后事务内仍应持锁，目录项 %d，错误 %v", lockedID, err)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return prepared, nil
	}

	// 2. 按新内容大小分流，锁读快照沿用原父目录、名称和版本。
	digest := sha256.Sum256([]byte("replacement preparation fixture"))
	for _, fixture := range []struct {
		name string
		size uint64
		plan domain.UploadPlan
	}{
		{"empty payload", 0, domain.UploadPlan{Mode: "SINGLE"}},
		{"last single byte", singleLimit, domain.UploadPlan{Mode: "SINGLE"}},
		{"first multipart byte", singleLimit + 1, domain.UploadPlan{Mode: "MULTIPART", PartSizeBytes: singleLimit, PartCount: 2, LastPartSizeBytes: 1}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			input := service.UploadReplacementDeclaration{EntryID: 31, ExpectedRevision: 6, SizeBytes: fixture.size, ExpectedSHA256: digest}
			prepared, err := read(t, input, fixture.size == 0)
			want := service.UploadReplacementPreparation{Declaration: input, ParentID: 33, Name: "发布笔记.MD", Plan: fixture.plan}
			if err != nil || prepared == nil || *prepared != want {
				t.Fatalf("准备结果不符 %+v，错误 %v", prepared, err)
			}
		})
	}

	// 3. 可见性和版本错误先于上传规划，合法目标才核对实际大小上限。
	for _, fixture := range []struct {
		name              string
		entryID, revision uint64
		want              error
	}{
		{"missing", 999, 1, domain.ErrFileEntryNotFound},
		{"another account", 32, 1, domain.ErrFileEntryNotFound},
		{"hidden ancestor", 35, 1, domain.ErrFileEntryNotFound},
		{"visible directory", 33, 1, domain.ErrFileEntryNotFile},
		{"stale file revision", 31, 5, domain.ErrEntryRevisionConflict},
		{"maximum revision", 36, math.MaxUint64, domain.ErrEntryRevisionExhausted},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			input := service.UploadReplacementDeclaration{EntryID: fixture.entryID, ExpectedRevision: fixture.revision, SizeBytes: storageLimit + 1, ExpectedSHA256: digest}
			prepared, err := read(t, input, false)
			if prepared != nil || !errors.Is(err, fixture.want) {
				t.Fatalf("预期拒绝 %v，得到 %+v，错误 %v", fixture.want, prepared, err)
			}
			var conflict *domain.EntryRevisionConflictError
			if errors.As(err, &conflict) != (fixture.want == domain.ErrEntryRevisionConflict) ||
				(conflict != nil && conflict.CurrentRevision != 6) {
				t.Fatalf("当前版本交付不符 %v", err)
			}
		})
	}
	t.Run("storage limit", func(t *testing.T) {
		input := service.UploadReplacementDeclaration{EntryID: 31, ExpectedRevision: 6, SizeBytes: storageLimit + 1, ExpectedSHA256: digest}
		prepared, err := read(t, input, false)
		var limitError *domain.UploadSizeLimitError
		if prepared != nil || !errors.As(err, &limitError) || limitError.MaxSizeBytes != storageLimit {
			t.Fatalf("存储限制结果不符 %+v，错误 %v", prepared, err)
		}
	})

	// 4. 准备过程没有改变文件、用户配额、任务、对象、历史或变更记录。
	var state string
	if err := pool.QueryRowContext(ctx, `SELECT CONCAT_WS('|',e.parent_id,e.name,e.revision,e.object_id,e.file_size,u.used_storage,u.reserved_storage,u.change_seq,
		(SELECT COUNT(*) FROM upload_tasks),(SELECT COUNT(*) FROM stored_objects),(SELECT COUNT(*) FROM file_versions),(SELECT COUNT(*) FROM change_events))
		FROM file_entries e JOIN users u ON u.id=e.user_id WHERE e.id=31`).
		Scan(&state); err != nil || state != "33|发布笔记.MD|6|29|16777216|16777216|512|0|1|3|0|0" {
		t.Fatalf("只读准备改变了业务记录 %q，错误 %v", state, err)
	}
}
