package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func trashEntryIdempotencyTestDatabase(t *testing.T, ctx context.Context) (*sql.DB, *Store, domain.Actor) {
	t.Helper()
	// 1. 在独占 MySQL 库安装正式幂等记录表。
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE idempotency_records (")
	if start < 0 {
		t.Fatal("正式幂等记录表未找到")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:]); err != nil {
		t.Fatal(err)
	}
	return pool, store, actor
}

func TestRestoreTrashEntryDataIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryIdempotencyTestDatabase(t, ctx)
	for _, fixture := range []struct {
		key               string
		entryID, revision uint64
	}{
		{"trash-file-001", 13, 3},
		{"trash-directory-001", 12, 1},
	} {
		t.Run(fixture.key, func(t *testing.T) {
			// 2. 使用当前事务保存真实回收结果与七字段 JSON。
			var expected *dto.TrashEntryData
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				trash := service.NewTrashEntryService(func(ctx context.Context, _ domain.Actor, action func(context.Context, service.TrashEntryTransaction) error) error {
					return action(ctx, txStore)
				})
				result, err := trash.Trash(txContext, actor, fixture.entryID, fixture.revision)
				if err != nil {
					return err
				}
				expected, err = dto.NewTrashEntryData(result)
				if err != nil {
					return err
				}
				encoded, err := json.Marshal(expected)
				if err != nil {
					return err
				}
				hash, err := service.BuildTrashEntryRequestHash(fixture.entryID, fixture.revision)
				if err != nil {
					return err
				}
				_, err = txStore.CreateUserIdempotencyRecord(txContext, domain.IdempotencyResultInput{
					Operation: domain.TrashEntryOperation, IdempotencyKey: fixture.key, RequestHash: hash,
					ResourceType: "ENTRY", ResourceID: &fixture.entryID, ResponseStatus: 200, ResponseBody: encoded,
					ExpiresAt: time.Now().Add(8 * 24 * time.Hour),
				})
				return err
			})
			if err != nil {
				t.Fatal("回收快照夹具事务失败", err)
			}

			// 3. 核对真实幂等记录与当前请求的关联，再恢复快照。
			committed, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.TrashEntryOperation, fixture.key)
			if err != nil {
				t.Fatal("无法读回已保存的回收快照", err)
			}
			digest, err := service.BuildTrashEntryRequestHash(fixture.entryID, fixture.revision)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if err := service.ValidateTrashEntryReplay(committed, actor.UserID, fixture.entryID, fixture.revision, fixture.key, digest, now); err != nil {
				t.Fatal("已提交回收记录未通过复用校验", err)
			}
			restored, err := dto.RestoreTrashEntryData(committed.ResponseBody)
			if err != nil || !reflect.DeepEqual(restored, expected) {
				t.Fatal("MySQL JSON 列读回内容未恢复为原快照", restored, err)
			}
			differentDigest, err := service.BuildTrashEntryRequestHash(fixture.entryID, fixture.revision+1)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.ValidateTrashEntryReplay(committed, actor.UserID, fixture.entryID, fixture.revision+1, fixture.key, differentDigest, now); !errors.Is(err, domain.ErrIdempotencyRequestConflict) {
				t.Fatal("已保存记录应拒绝同键异参", err)
			}
			if err := service.ValidateTrashEntryReplay(committed, actor.UserID, fixture.entryID, fixture.revision+1, fixture.key, differentDigest, committed.ExpiresAt); !errors.Is(err, domain.ErrIdempotencyRecordExpired) {
				t.Fatal("保留期限到点应先返回过期", err)
			}

			// 4. 在独占库改动保存版本，拒绝无法对应本次请求的成功快照。
			update, err := pool.ExecContext(ctx, `UPDATE idempotency_records
				SET response_body=JSON_SET(response_body,'$.entry.revision','99')
				WHERE user_id=? AND operation=? AND idempotency_key=?`, actor.UserID, domain.TrashEntryOperation, fixture.key)
			if err != nil {
				t.Fatal(err)
			}
			if count, err := update.RowsAffected(); err != nil || count != 1 {
				t.Fatal("未更新本轮创建的回收快照", count, err)
			}
			wrongRevision, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.TrashEntryOperation, fixture.key)
			if err != nil {
				t.Fatal("无法读回版本异常的回收快照", err)
			}
			if err := service.ValidateTrashEntryReplay(wrongRevision, actor.UserID, fixture.entryID, fixture.revision, fixture.key, digest, now); !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatal("已保存版本异常未阻止复用", err)
			}

			// 5. 去掉回收时间，确认完整快照仍需恢复器检查。
			changed, err := pool.ExecContext(ctx, `UPDATE idempotency_records
				SET response_body=JSON_REMOVE(response_body,'$.entry.deleted_at')
				WHERE user_id=? AND operation=? AND idempotency_key=?`, actor.UserID, domain.TrashEntryOperation, fixture.key)
			if err != nil {
				t.Fatal(err)
			}
			if count, err := changed.RowsAffected(); err != nil || count != 1 {
				t.Fatal("未更新本轮创建的异常快照", count, err)
			}
			broken, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.TrashEntryOperation, fixture.key)
			if err != nil {
				t.Fatal("无法读取异常快照夹具", err)
			}
			if invalid, err := dto.RestoreTrashEntryData(broken.ResponseBody); invalid != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatal("缺失回收时间的持久快照未拒绝", invalid, err)
			}
		})
	}
}
