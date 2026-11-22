package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestWithMultipartUploadExpirationTransactionIntegration(t *testing.T) {
	ctx, pool, store := prepareMultipartCompletionDatabase(t)
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	reset := func(t *testing.T, status domain.UploadStatus) {
		t.Helper()
		resetMultipartUploadCancellationFixture(t, ctx, pool)
		execute(t, "UPDATE upload_tasks SET status=? WHERE id=19", status)
	}
	scan := func(t *testing.T, status domain.UploadStatus) domain.UploadExpirationCandidate {
		t.Helper()
		candidates, err := store.ListMultipartUploadExpirationCandidates(ctx, 1)
		if err != nil || len(candidates) != 1 {
			t.Fatal("未取得本轮专用分片到期候选", candidates, err)
		}
		candidate := candidates[0]
		want := domain.UploadExpirationCandidate{TaskID: 19, UserID: 7, StagedObjectID: 23, Status: status,
			LeaseVersion: 4, ExpiresAt: candidate.ExpiresAt, UploadMode: "MULTIPART"}
		if candidate != want {
			t.Fatal("专用分片到期候选的归属、阶段或版本不符", candidate)
		}
		return candidate
	}
	type facts struct {
		task           *domain.UploadTask
		object         *domain.StoredObject
		used, reserved uint64
	}
	read := func(t *testing.T) facts {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		object, err := store.FindUserStoredObject(ctx, 7, 23)
		if err != nil {
			t.Fatal(err)
		}
		current := facts{task: task, object: object}
		if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&current.used, &current.reserved); err != nil {
			t.Fatal(err)
		}
		return current
	}

	// 1. 用第二连接确认账号、任务与暂存对象都持有真实行锁。
	t.Run("locks account task and original verifying object", func(t *testing.T) {
		reset(t, domain.UploadStatusVerifying)
		execute(t, "UPDATE stored_objects SET status='VERIFYING',storage_etag='fixture-etag',storage_version_id='fixture-version' WHERE id=23")
		candidate, before := scan(t, domain.UploadStatusVerifying), read(t)
		calls := 0
		err := store.WithMultipartUploadExpirationTransaction(ctx, candidate, func(txContext context.Context, txStore *Store, task *domain.UploadTask, object *domain.StoredObject) error {
			calls++
			if txStore.loginUserID != candidate.UserID || !reflect.DeepEqual(task, before.task) || !reflect.DeepEqual(object, before.object) ||
				task.StorageUploadID == nil || *task.StorageUploadID != multipartCancellationFixtureSession {
				t.Fatal("锁内记录未保留原会话、对象或账号")
			}
			contender, err := pool.BeginTx(txContext, nil)
			if err != nil {
				return err
			}
			defer contender.Rollback()
			for _, statement := range []string{
				"SELECT id FROM users WHERE id=7 FOR UPDATE NOWAIT",
				"SELECT id FROM upload_tasks WHERE id=19 FOR UPDATE NOWAIT",
				"SELECT id FROM stored_objects WHERE id=23 FOR UPDATE NOWAIT",
			} {
				var id uint64
				var lockError *mysql.MySQLError
				if err := contender.QueryRowContext(txContext, statement).Scan(&id); !errors.As(err, &lockError) || lockError.Number != 3572 {
					t.Fatal("另一连接未被行锁拦住", statement, err)
				}
			}
			return nil
		})
		if err != nil || calls != 1 || !reflect.DeepEqual(read(t), before) {
			t.Fatal("到期复核未执行一次回调或改变了数据库", calls, err)
		}
		t.Log("第二连接对账号、任务和暂存对象的 NOWAIT 均返回 3572，原会话尾部空格保持")
	})

	// 2. 继续处理恰好到点的清理，保留未绑定会话与禁用账号已有任务。
	t.Run("due task lease and retry at database time", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		now := time.Now().UTC().Truncate(time.Second)
		execute(t, "SET timestamp = ?", now.Unix())
		defer execute(t, "SET timestamp = DEFAULT")
		reset(t, domain.UploadStatusExpiring)
		execute(t, `UPDATE upload_tasks SET storage_upload_id=NULL,expires_at=UTC_TIMESTAMP(6),
			lease_owner='old-cleanup',lease_until=UTC_TIMESTAMP(6),next_attempt_at=UTC_TIMESTAMP(6) WHERE id=19`)
		execute(t, "UPDATE users SET status='DISABLED' WHERE id=7")
		candidate, before := scan(t, domain.UploadStatusExpiring), read(t)
		callbackError := errors.New("fixture callback stopped")
		err := store.WithMultipartUploadExpirationTransaction(ctx, candidate, func(_ context.Context, _ *Store, task *domain.UploadTask, object *domain.StoredObject) error {
			if !task.ExpiresAt.Equal(now) || task.StorageUploadID != nil || !reflect.DeepEqual(task, before.task) || !reflect.DeepEqual(object, before.object) {
				t.Fatal("到点的未绑定任务没有保留锁内事实")
			}
			return callbackError
		})
		if !errors.Is(err, callbackError) || !reflect.DeepEqual(read(t), before) {
			t.Fatal("复核未保留回调错误或数据库状态", err)
		}
	})

	// 3. 拒绝扫描后的竞争变化和无效绑定，保持回调前的状态与账目。
	for _, scenario := range []struct {
		name, change string
		want         error
	}{
		{"cancellation took task", "UPDATE upload_tasks SET status='CANCELLING' WHERE id=19", domain.ErrUploadTaskNotReady},
		{"phase changed", "UPDATE upload_tasks SET status='VERIFYING' WHERE id=19", domain.ErrUploadTaskBusy},
		{"mode changed", "UPDATE upload_tasks SET upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL WHERE id=19", domain.ErrUploadTaskBusy},
		{"deadline extended", "UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19", domain.ErrUploadTaskBusy},
		{"replaced lease", "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", domain.ErrUploadTaskBusy},
		{"active lease", "UPDATE upload_tasks SET lease_owner='active',lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE WHERE id=19", domain.ErrUploadTaskBusy},
		{"retry postponed", "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE WHERE id=19", domain.ErrUploadTaskNotReady},
		{"object reference changed", "UPDATE upload_tasks SET staged_object_id=29 WHERE id=19", domain.ErrUploadTaskBusy},
		{"invalid session", "UPDATE upload_tasks SET storage_upload_id=' ' WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"invalid part layout", "UPDATE upload_tasks SET part_count=3 WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"invalid reservation", "UPDATE upload_tasks SET reserved_bytes=reserved_bytes-1 WHERE id=19", domain.ErrInvalidUploadTaskData},
		{"object size changed", "UPDATE stored_objects SET file_size=file_size+1 WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"already published object", "UPDATE stored_objects SET status='READY',verified_sha256=UNHEX(REPEAT('ac',32)),verified_at=UTC_TIMESTAMP(6) WHERE id=23", domain.ErrInvalidStoredObjectData},
		{"other account", "", domain.ErrUploadTaskNotFound},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			reset(t, domain.UploadStatusExpiring)
			candidate := scan(t, domain.UploadStatusExpiring)
			if scenario.change == "" {
				candidate.UserID = 8
			} else {
				execute(t, scenario.change)
			}
			before := read(t)
			err := store.WithMultipartUploadExpirationTransaction(ctx, candidate, func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error {
				t.Error("过时或无效的候选进入了回调")
				return nil
			})
			if !errors.Is(err, scenario.want) || !reflect.DeepEqual(read(t), before) {
				t.Fatal("拒绝结果或只读事务边界不符", scenario.want, err)
			}
		})
	}
}
