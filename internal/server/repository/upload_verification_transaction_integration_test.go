package repository

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestWithUploadVerificationTransactionIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(t *testing.T, statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	read := func(t *testing.T) (*domain.UploadTask, *domain.StoredObject) {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		object, err := store.FindUserStoredObject(ctx, 7, 23)
		if err != nil {
			t.Fatal(err)
		}
		return task, object
	}
	reset := func(t *testing.T) (*domain.UploadTask, *domain.StoredObject, domain.UploadContentDigest) {
		t.Helper()
		execute(t, "UPDATE upload_tasks SET status='VERIFYING',upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,reserved_bytes=file_size,lease_owner='worker',lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE,lease_version=3,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY,next_attempt_at=NULL,updated_at=UTC_TIMESTAMP(6) WHERE id=19")
		execute(t, "UPDATE stored_objects SET status='VERIFYING',storage_etag='fixture-etag',storage_version_id='null' WHERE id=23")
		task, object := read(t)
		return task, object, domain.UploadContentDigest{SizeBytes: task.FileSize, SHA256: *task.ExpectedSHA256}
	}
	t.Run("rows locked until callback exits", func(t *testing.T) {
		task, object, digest := reset(t)
		err := store.WithUploadVerificationTransaction(ctx, task, object, digest, func(txContext context.Context, _ *Store, current *domain.UploadTask, staged *domain.StoredObject) error {
			if !reflect.DeepEqual(current, task) || !reflect.DeepEqual(staged, object) {
				t.Fatal("当前读回调结果错误")
			}
			// 另一条连接在回调期间立即试锁，三个记录都应被当前事务占有。
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
				err := contender.QueryRowContext(txContext, statement).Scan(&id)
				var lockError *mysql.MySQLError
				if !errors.As(err, &lockError) || lockError.Number != 3572 {
					t.Fatal("另一连接未被预期行锁拒绝", statement, err)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		after, staged := read(t)
		if !reflect.DeepEqual(after, task) || !reflect.DeepEqual(staged, object) {
			t.Fatal("只读复核改变了任务或对象")
		}
	})
	t.Run("callback writes roll back", func(t *testing.T) {
		task, object, digest := reset(t)
		failure := errors.New("fixture write aborted")
		err := store.WithUploadVerificationTransaction(ctx, task, object, digest, func(txContext context.Context, txStore *Store, current *domain.UploadTask, _ *domain.StoredObject) error {
			result := txStore.database.WithContext(txContext).Exec("UPDATE upload_tasks SET attempt_count=attempt_count+1 WHERE user_id=? AND id=?", current.UserID, current.ID)
			if result.Error != nil || result.RowsAffected != 1 {
				t.Fatal("联测未写入预期的单行", result.Error)
			}
			return failure
		})
		after, staged := read(t)
		if !errors.Is(err, failure) || !reflect.DeepEqual(after, task) || !reflect.DeepEqual(staged, object) {
			t.Fatal("复核回调失败后未完整回滚", err)
		}
	})
	t.Run("new lease rejects old verification", func(t *testing.T) {
		task, object, digest := reset(t)
		execute(t, "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19")
		candidate := domain.UploadVerificationCandidate{TaskID: task.ID, UserID: task.UserID, StagedObjectID: task.StagedObjectID, LeaseVersion: task.LeaseVersion}
		newLease, err := store.ClaimUploadVerificationLease(ctx, candidate, "new-worker")
		if err != nil || newLease == nil || newLease.LeaseVersion != task.LeaseVersion+1 {
			t.Fatal("真实新租约领取失败", err)
		}
		err = store.WithUploadVerificationTransaction(ctx, task, object, digest, func(context.Context, *Store, *domain.UploadTask, *domain.StoredObject) error {
			t.Fatal("旧租约取得了写回机会")
			return nil
		})
		after, staged := read(t)
		if !errors.Is(err, domain.ErrUploadTaskBusy) || !reflect.DeepEqual(after, newLease) || !reflect.DeepEqual(staged, object) {
			t.Fatal("旧结果未拒绝或改变了新租约", err)
		}
	})
	t.Run("read upload source device", func(t *testing.T) {
		task, staged, digest := reset(t)
		rollbackFixture := errors.New("rollback fixture device update and object selection")
		err := store.WithUploadVerificationTransaction(ctx, task, staged, digest, func(txContext context.Context, txStore *Store, currentTask *domain.UploadTask, currentObject *domain.StoredObject) error {
			source, err := txStore.FindUploadSourceDevice(txContext, currentTask)
			if err != nil || source == nil || source.ID != task.DeviceID || source.UserID != task.UserID || source.Name != "fixture" || source.RevokedAt != nil {
				t.Fatal("未读到任务的来源设备", source, err)
			}
			// 其他账号的设备与不存在的设备都按当前账号范围返回缺失。
			for _, deviceID := range []uint64{12, 404} {
				otherTask := *currentTask
				otherTask.DeviceID = deviceID
				device, err := txStore.FindUploadSourceDevice(txContext, &otherTask)
				if device != nil || !errors.Is(err, domain.ErrDeviceNotFound) {
					t.Fatal("未按用户隔离来源设备", device, err)
				}
			}

			// 在隔离库模拟设备已改名和撤销，复查后的名称用于写入输入。
			const renamed = " 新设备名 🌱 "
			result := txStore.database.WithContext(txContext).Exec(
				"UPDATE user_devices SET name=?, revoked_at=UTC_TIMESTAMP(6) WHERE user_id=? AND id=?", renamed, task.UserID, task.DeviceID)
			if result.Error != nil || result.RowsAffected != 1 {
				t.Fatal("设置设备联测夹具失败", result.Error)
			}
			updated, err := txStore.FindUploadSourceDevice(txContext, currentTask)
			if err != nil || updated == nil || updated.Name != renamed || updated.RevokedAt == nil || source.Name != "fixture" || source.RevokedAt != nil {
				t.Fatal("设备名称快照或撤销记录错误", updated, err)
			}
			selected, err := txStore.SelectUploadPublicationObject(txContext, currentTask, currentObject, digest)
			if err != nil {
				return err
			}
			input, err := service.BuildPublishedFileEntryInput(currentTask, selected, currentTask.RequestedName, updated)
			if err != nil || input == nil || input.SourceDeviceName != renamed || input.SourceDeviceID != task.DeviceID {
				t.Fatal("来源设备未正确传给发布写入输入", input, err)
			}
			return rollbackFixture
		})
		if !errors.Is(err, rollbackFixture) {
			t.Fatal("读取来源设备联测失败", err)
		}
	})
	t.Run("select publication object", func(t *testing.T) {
		rollbackFailure := errors.New("fixture aborts after object selection")
		for _, scenario := range []struct {
			name     string
			reuse    bool
			rollback bool
		}{
			{name: "existing ready object", reuse: true},
			{name: "new ready object rolled back", rollback: true},
			{name: "new ready object committed"},
		} {
			t.Run(scenario.name, func(t *testing.T) {
				task, staged, digest := reset(t)
				wantID := staged.ID
				if scenario.reuse {
					// 对象 29 是同账号已有的 READY 夹具，使用它的摘要触发复用。
					wantID = 29
					originalHash := *task.ExpectedSHA256
					t.Cleanup(func() {
						if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET expected_sha256=? WHERE id=19", originalHash[:]); err != nil {
							t.Error(err)
						}
					})
					execute(t, "UPDATE upload_tasks SET expected_sha256=UNHEX(REPEAT('00',32)) WHERE id=19")
					task, staged = read(t)
					digest.SHA256 = *task.ExpectedSHA256
				}
				var selected *domain.StoredObject
				err := store.WithUploadVerificationTransaction(ctx, task, staged, digest, func(txContext context.Context, txStore *Store, currentTask *domain.UploadTask, currentObject *domain.StoredObject) error {
					object, err := txStore.SelectUploadPublicationObject(txContext, currentTask, currentObject, digest)
					if err != nil {
						return err
					}
					if object == nil || object.ID != wantID || object.Status != domain.StoredObjectStatusReady ||
						object.VerifiedSHA256 == nil || *object.VerifiedSHA256 != digest.SHA256 {
						t.Fatal("选择的 READY 对象错误", object)
					}
					selected = object
					if scenario.rollback {
						return rollbackFailure
					}
					return nil
				})
				var expectedError error
				if scenario.rollback {
					expectedError = rollbackFailure
				}
				if !errors.Is(err, expectedError) || selected == nil {
					t.Fatal("发布对象选择事务失败", err)
				}
				savedTask, savedStaged := read(t)
				wantStaged := selected
				if scenario.reuse || scenario.rollback {
					wantStaged = staged
				}
				if !reflect.DeepEqual(savedStaged, wantStaged) || !reflect.DeepEqual(savedTask, task) {
					t.Fatal("对象选择提交或回滚后的持久记录错误")
				}
			})
		}
	})
}
