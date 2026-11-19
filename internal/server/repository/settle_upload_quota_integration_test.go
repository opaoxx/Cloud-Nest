package repository

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestSettlePublishedUploadQuotaIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	type quotaState struct {
		used, reserved, quota, sequence uint64
		updatedAt                       time.Time
	}
	readQuota := func(t *testing.T, userID uint64) quotaState {
		t.Helper()
		var state quotaState
		err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage,storage_quota,change_seq,updated_at FROM users WHERE id=?", userID).
			Scan(&state.used, &state.reserved, &state.quota, &state.sequence, &state.updatedAt)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	const size = domain.MaxSingleUploadSizeBytes
	rollbackFixture := errors.New("fixture rolls back file and quota settlement")
	otherAccount := readQuota(t, 8)
	for _, scenario := range []struct {
		name                     string
		fileSize, used, reserved uint64
		publish                  bool
		retry                    bool
		changeSQL                string
		wantError                error
	}{
		{name: "reused file and repeated settlement", fileSize: size, used: size, reserved: size + 512, publish: true, retry: true},
		{name: "new file and quota rolled back together", fileSize: size, used: size, reserved: size + 512, publish: true, wantError: rollbackFixture},
		{name: "zero bytes at full quota", used: size},
		{name: "maximum unsigned used storage", fileSize: size, used: math.MaxUint64 - size, reserved: size},
		{name: "insufficient aggregate reservation", fileSize: size, used: size, reserved: size - 1, wantError: domain.ErrInvalidAccountData},
		{name: "task reservation has changed", fileSize: size, used: size, reserved: size,
			changeSQL: "UPDATE upload_tasks SET reserved_bytes=reserved_bytes-1 WHERE id=19", wantError: domain.ErrInvalidUploadTaskData},
		{name: "stale lease", fileSize: size, used: size, reserved: size,
			changeSQL: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "lease deadline reached", fileSize: size, used: size, reserved: size,
			changeSQL: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备本次任务的完整预占、账户配额和校验租约。
			digest := domain.UploadContentDigest{SizeBytes: scenario.fileSize}
			if scenario.wantError == rollbackFixture {
				digest.SHA256[0] = 0xff
			}
			execute(t, "UPDATE users SET storage_quota=?,used_storage=?,reserved_storage=?,updated_at='2026-11-17 08:00:00.123456' WHERE id=7",
				scenario.used+scenario.reserved, scenario.used, scenario.reserved)
			execute(t, "UPDATE stored_objects SET status='VERIFYING',file_size=?,verified_sha256=NULL,verified_at=NULL WHERE id=23", scenario.fileSize)
			execute(t, `UPDATE upload_tasks SET status='VERIFYING',upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,
				published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
				file_size=?,reserved_bytes=?,expected_sha256=?,lease_owner='verify-worker',lease_version=3,
				lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,updated_at=UTC_TIMESTAMP(6)
				WHERE id=19`, scenario.fileSize, scenario.fileSize, digest.SHA256[:])
			task, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			staged, err := store.FindUserStoredObject(ctx, 7, 23)
			if err != nil {
				t.Fatal(err)
			}
			before := readQuota(t, 7)

			// 2. 在真实复核事务中结算，验证可与文件新建共用同一事务。
			var fileInput *domain.CreatePublishedFileEntryInput
			err = store.WithUploadVerificationTransaction(ctx, task, staged, digest, func(txContext context.Context, txStore *Store, current *domain.UploadTask, currentObject *domain.StoredObject) error {
				if scenario.publish {
					selected, err := txStore.SelectUploadPublicationObject(txContext, current, currentObject, digest)
					if err != nil {
						return err
					}
					source, err := txStore.FindUploadSourceDevice(txContext, current)
					if err != nil {
						return err
					}
					fileInput, err = service.BuildPublishedFileEntryInput(current, selected, scenario.name+".PDF", source)
					if err != nil {
						return err
					}
					if _, err := txStore.CreatePublishedFileEntry(txContext, current, *fileInput); err != nil {
						return err
					}
				}
				if scenario.changeSQL != "" {
					result := txStore.database.WithContext(txContext).Exec(scenario.changeSQL)
					if result.Error != nil || result.RowsAffected != 1 {
						t.Fatal("结算条件变化夹具设置失败", result.Error)
					}
				}
				if err := txStore.SettlePublishedUploadQuota(txContext, current); err != nil {
					return err
				}
				if scenario.retry {
					if err := txStore.SettlePublishedUploadQuota(txContext, current); !errors.Is(err, domain.ErrInvalidUploadTaskData) {
						t.Fatal("非空任务第二次结算应被拒绝", err)
					}
				}
				if scenario.wantError == rollbackFixture {
					return rollbackFixture
				}
				return nil
			})
			if !errors.Is(err, scenario.wantError) {
				t.Fatal("真实结算错误不符", err)
			}

			// 3. 核对任务和账号的账目，以及失败时的整体回滚。
			savedTask, err := store.FindUserUploadTask(ctx, 7, 19)
			if err != nil {
				t.Fatal(err)
			}
			after := readQuota(t, 7)
			if scenario.wantError != nil || scenario.fileSize == 0 {
				if after != before || !reflect.DeepEqual(savedTask, task) {
					t.Fatal("失败、回滚或零字节改变了配额或任务", after)
				}
			} else {
				expectedTask := *task
				expectedTask.ReservedBytes, expectedTask.UpdatedAt = 0, after.updatedAt
				if after.used != before.used+scenario.fileSize || after.reserved != before.reserved-scenario.fileSize ||
					after.quota != before.quota || after.sequence != before.sequence || after.updatedAt.Equal(before.updatedAt) ||
					!reflect.DeepEqual(savedTask, &expectedTask) {
					t.Fatal("用户预占与任务预占没有等额转入已用", after, savedTask)
				}
			}
			if fileInput != nil {
				entry, err := store.FindUserActiveFileEntryByName(ctx, 7, fileInput.ParentID, fileInput.NameKey)
				if scenario.wantError == nil && (entry == nil || err != nil) ||
					scenario.wantError != nil && (entry != nil || !errors.Is(err, domain.ErrFileEntryNotFound)) {
					t.Fatal("文件与配额提交结果不一致", entry, err)
				}
			}
			if scenario.wantError == rollbackFixture {
				afterObject, err := store.FindUserStoredObject(ctx, 7, 23)
				if err != nil || !reflect.DeepEqual(afterObject, staged) {
					t.Fatal("同一事务中的对象推进没有一起回滚", afterObject, err)
				}
			}
			if readQuota(t, 8) != otherAccount {
				t.Fatal("结算改变了其他账号的配额")
			}
		})
	}
}
