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
	for _, mode := range []string{"SINGLE", "MULTIPART"} {
		t.Run(mode, func(t *testing.T) { verifyPublishedUploadQuota(t, mode) })
	}
}

func verifyPublishedUploadQuota(t *testing.T, mode string) {
	t.Helper()
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
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
	size := domain.MaxSingleUploadSizeBytes
	var session, partSize, partCount, objectVersion, etag any
	if mode == "MULTIPART" {
		size++
		session, partSize, partCount = "fixture-settle-session ", domain.MaxSingleUploadSizeBytes, 2
		objectVersion, etag = "merged-version", "merged-etag"
		execute(t, "UPDATE stored_objects SET file_size=? WHERE id=29", size)
		execute(t, "UPDATE file_entries SET file_size=? WHERE id=31", size)
	}
	rollbackFixture := errors.New("fixture rolls back file and quota settlement")
	otherAccount := readQuota(t, 8)
	type quotaScenario struct {
		name                     string
		fileSize, used, reserved uint64
		publish                  bool
		retry                    bool
		changeSQL                string
		wantError                error
	}
	scenarios := []quotaScenario{
		{name: "reused file and repeated settlement", fileSize: size, used: size, reserved: size + 512, publish: true, retry: true},
		{name: "new file and quota rolled back together", fileSize: size, used: size, reserved: size + 512, publish: true, wantError: rollbackFixture},
		{name: "zero bytes at full quota", used: size},
		{name: "maximum unsigned used storage", fileSize: size, used: math.MaxUint64 - size, reserved: size},
		{name: "insufficient aggregate reservation", fileSize: size, used: size, reserved: size - 1, publish: true, wantError: domain.ErrInvalidAccountData},
		{name: "task reservation has changed", fileSize: size, used: size, reserved: size,
			changeSQL: "UPDATE upload_tasks SET reserved_bytes=reserved_bytes-1 WHERE id=19", wantError: domain.ErrInvalidUploadTaskData},
		{name: "stale lease", fileSize: size, used: size, reserved: size,
			changeSQL: "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19", wantError: domain.ErrUploadTaskBusy},
		{name: "lease deadline reached", fileSize: size, used: size, reserved: size,
			changeSQL: "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=19", wantError: domain.ErrUploadTaskBusy},
	}
	if mode == "MULTIPART" {
		scenarios = append(scenarios, quotaScenario{name: "original session changes before settlement", fileSize: size, used: size, reserved: size, publish: true,
			changeSQL: "UPDATE upload_tasks SET storage_upload_id=RTRIM(storage_upload_id) WHERE id=19", wantError: domain.ErrUploadTaskBusy})
	}
	for _, scenario := range scenarios {
		if mode == "MULTIPART" && scenario.fileSize == 0 {
			continue
		}
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备本次任务的完整预占、账户配额和校验租约。
			digest := domain.UploadContentDigest{SizeBytes: scenario.fileSize}
			if scenario.wantError == rollbackFixture {
				digest.SHA256[0] = 0xff
			}
			execute(t, "UPDATE users SET storage_quota=?,used_storage=?,reserved_storage=?,updated_at='2026-11-17 08:00:00.123456' WHERE id=7",
				scenario.used+scenario.reserved, scenario.used, scenario.reserved)
			execute(t, "UPDATE stored_objects SET status='VERIFYING',file_size=?,verified_sha256=NULL,verified_at=NULL,storage_version_id=?,storage_etag=? WHERE id=23",
				scenario.fileSize, objectVersion, etag)
			execute(t, `UPDATE upload_tasks SET status='VERIFYING',upload_mode=?,storage_upload_id=?,part_size=?,part_count=?,
				published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
				file_size=?,reserved_bytes=?,expected_sha256=?,lease_owner='verify-worker',lease_version=3,
				lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,updated_at=UTC_TIMESTAMP(6)
				WHERE id=19`, mode, session, partSize, partCount, scenario.fileSize, scenario.fileSize, digest.SHA256[:])
			if mode == "MULTIPART" {
				execute(t, "UPDATE upload_tasks SET lease_owner=NULL,lease_until=NULL,next_attempt_at=NULL WHERE id=19")
				candidates, err := store.ListMultipartUploadVerificationCandidates(ctx, 1)
				if err != nil || len(candidates) != 1 || candidates[0].TaskID != 19 {
					t.Fatal("未找到待校验分片任务", candidates, err)
				}
				if claimed, err := store.ClaimMultipartUploadVerificationLease(ctx, candidates[0], "verify-worker"); err != nil || claimed == nil {
					t.Fatal("未取得分片校验租约", err)
				}
			}
			var otherTask *domain.UploadTask
			if scenario.retry {
				execute(t, `INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size)
					VALUES(30,7,'local_minio','fixture-bucket','other-reserved-upload',512)`)
				execute(t, `INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at)
					VALUES(20,7,11,30,13,'other.bin','SINGLE',512,'UPLOADING',512,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`)
				t.Cleanup(func() {
					execute(t, "DELETE FROM upload_tasks WHERE user_id=7 AND id=20")
					execute(t, "DELETE FROM stored_objects WHERE owner_user_id=7 AND id=30")
				})
				var err error
				otherTask, err = store.FindUserUploadTask(ctx, 7, 20)
				if err != nil {
					t.Fatal(err)
				}
			}
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
					if scenario.retry && selected.ID != 29 {
						t.Fatal("未选择预期的同账号去重对象", selected.ID)
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
			if scenario.retry {
				// 在另一事务重试，确认已提交账目和另一条待上传任务都保持原值。
				err := store.withActiveUserTransaction(ctx, task.UserID, func(txContext context.Context, txStore *Store) error {
					return txStore.SettlePublishedUploadQuota(txContext, task)
				})
				if !errors.Is(err, domain.ErrInvalidUploadTaskData) || readQuota(t, 7) != after {
					t.Fatal("旧快照跨事务重复结算改变了账号账目", err)
				}
				current, err := store.FindUserUploadTask(ctx, 7, 20)
				if err != nil || !reflect.DeepEqual(current, otherTask) || after.reserved != 512 {
					t.Fatal("结算改变了另一任务的 512 字节预占", err)
				}
				t.Logf("%s 结算 %d 字节，账号预占 %d → %d，已用 %d → %d", mode, scenario.fileSize, before.reserved, after.reserved, before.used, after.used)
			}
			if readQuota(t, 8) != otherAccount {
				t.Fatal("结算改变了其他账号的配额")
			}
		})
	}
}
