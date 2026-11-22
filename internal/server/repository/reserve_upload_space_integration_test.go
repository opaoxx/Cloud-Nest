package repository

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestReserveUserUploadSpaceIntegration(t *testing.T) {
	// 1. 准备独占库中的正式表与认证身份。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	for _, boundary := range []struct{ table, next string }{{"auth_sessions", "stored_objects"}, {"upload_tasks", "idempotency_records"}} {
		start := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.table+" (")
		end := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.next+" (")
		if start < 0 || end <= start {
			t.Fatal("正式配额联测表边界错误")
		}
		if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	installUploadReplacementFixtureColumns(t, ctx, pool)
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,storage_quota) VALUES(1,'quota_owner','配额用户','fixture_hash',100),(3,'quota_other','另一用户','fixture_hash',100)",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','配额设备','WEB'),(31,3,'38ee758a-0727-42de-ae7d-3bb33155e148','另一设备','WINDOWS')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),(37,3,31,UNHEX(REPEAT('33',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 13, SessionID: 17}
	type storageState struct {
		quota, used, reserved, sequence uint64
		updatedAt                       time.Time
	}
	readState := func(userID uint64) storageState {
		t.Helper()
		var state storageState
		if err := pool.QueryRowContext(ctx, "SELECT storage_quota,used_storage,reserved_storage,change_seq,updated_at FROM users WHERE id=?", userID).
			Scan(&state.quota, &state.used, &state.reserved, &state.sequence, &state.updatedAt); err != nil {
			t.Fatal(err)
		}
		return state
	}
	setUsage := func(quota, used, reserved uint64) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, "UPDATE users SET storage_quota=?,used_storage=?,reserved_storage=?,updated_at='2026-11-17 08:00:00.123456' WHERE id=1", quota, used, reserved); err != nil {
			t.Fatal(err)
		}
	}
	reserve := func(current domain.Actor, size uint64) error {
		return store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
			return txStore.ReserveUserUploadSpace(txContext, size)
		})
	}

	// 2. 在真实 BIGINT UNSIGNED 和 CHECK 约束下验证边界及零字节。
	maximum := ^uint64(0)
	for _, fixture := range []struct {
		name                        string
		quota, used, reserved, size uint64
		wantReserved                uint64
		wantError                   error
	}{
		{"ordinary", 100, 40, 20, 30, 50, nil},
		{"exact remaining", 100, 40, 20, 40, 60, nil},
		{"insufficient", 100, 40, 20, 41, 20, domain.ErrQuotaExceeded},
		{"zero quota zero file", 0, 0, 0, 0, 0, nil},
		{"full quota zero file", 100, 100, 0, 0, 0, nil},
		{"maximum reservation", maximum, 0, maximum - 1, 1, maximum, nil},
		{"maximum request", maximum, 0, 0, maximum, maximum, nil},
		{"addition would wrap", maximum, 0, 1, maximum, 1, domain.ErrQuotaExceeded},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			setUsage(fixture.quota, fixture.used, fixture.reserved)
			before := readState(1)
			err := reserve(actor, fixture.size)
			if (fixture.wantError == nil && err != nil) || (fixture.wantError != nil && !errors.Is(err, fixture.wantError)) {
				t.Fatal("真实预占边界错误", err)
			}
			after := readState(1)
			if after.quota != before.quota || after.used != before.used || after.sequence != before.sequence || after.reserved != fixture.wantReserved {
				t.Fatal("预占字段或其他用户事实错误")
			}
			if (fixture.size == 0 || fixture.wantError != nil) && !after.updatedAt.Equal(before.updatedAt) {
				t.Fatal("零字节或失败请求改变了用户时间")
			}
			if fixture.size > 0 && fixture.wantError == nil && (after.updatedAt.Equal(before.updatedAt) || after.updatedAt.Nanosecond()%1000 != 0) {
				t.Fatal("成功预占的数据库更新时间错误")
			}
		})
	}
	t.Run("single connection repeated current reads", func(t *testing.T) {
		setUsage(100, 10, 20)
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			var previous uint64
			if err := txStore.database.WithContext(txContext).Raw("SELECT reserved_storage FROM users WHERE id=1").Scan(&previous).Error; err != nil || previous != 20 {
				return errors.New("建立配额读取快照失败")
			}
			if err := txStore.ReserveUserUploadSpace(txContext, 10); err != nil {
				return err
			}
			return txStore.ReserveUserUploadSpace(txContext, 15)
		})
		if err != nil || readState(1).reserved != 45 {
			t.Fatal("单连接重复预占未正确累加", err)
		}
	})
	t.Run("outer error and cancellation roll back reservation", func(t *testing.T) {
		failure := errors.New("outer quota failure")
		for _, mode := range []string{"callback", "cancelled"} {
			setUsage(100, 0, 10)
			before := readState(1)
			cancelled, stop := context.WithCancel(ctx)
			err := store.WithUserTransaction(cancelled, actor, func(txContext context.Context, txStore *Store) error {
				if err := txStore.ReserveUserUploadSpace(txContext, 20); err != nil {
					return err
				}
				if mode == "cancelled" {
					stop()
					return nil
				}
				return failure
			})
			stop()
			wantError := failure
			if mode == "cancelled" {
				wantError = context.Canceled
			}
			if !errors.Is(err, wantError) || readState(1) != before {
				t.Fatal("外层失败未回滚配额和时间", err)
			}
		}
	})
	t.Run("ownership scope", func(t *testing.T) {
		setUsage(100, 0, 0)
		before := readState(1)
		other := domain.Actor{UserID: 3, DeviceID: 31, SessionID: 37}
		if err := reserve(other, 20); err != nil || readState(3).reserved != 20 || readState(1) != before {
			t.Fatal("预占用户范围错误", err)
		}
		if err := reserve(domain.Actor{UserID: 1, DeviceID: 13, SessionID: 37}, 1); !errors.Is(err, domain.ErrSessionNotFound) || readState(1) != before {
			t.Fatal("错误会话身份未阻止预占", err)
		}
	})
	for _, fixture := range []struct {
		name, change, restore string
		wantError             error
	}{
		{"disabled account", "UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
		{"revoked device", "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=13", "UPDATE user_devices SET revoked_at=NULL WHERE id=13", domain.ErrDeviceRevoked},
		{"revoked session", "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		{"expired session", "UPDATE auth_sessions SET expires_at='2000-01-01 00:00:00.000000' WHERE id=17", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=17", domain.ErrSessionExpired},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			before := readState(1)
			if _, err := pool.ExecContext(ctx, fixture.change); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := pool.ExecContext(ctx, fixture.restore); err != nil {
					t.Error(err)
				}
			}()
			if err := reserve(actor, 1); !errors.Is(err, fixture.wantError) || readState(1).reserved != before.reserved {
				t.Fatal("认证状态变化未阻止预占", err)
			}
		})
	}

	// 3. 并发请求共享用户锁，只有剩余配额允许的请求可以提交。
	t.Run("concurrent requests cannot overbook", func(t *testing.T) {
		setUsage(100, 0, 0)
		var workers sync.WaitGroup
		start := make(chan struct{})
		results := make(chan error, 2)
		for range 2 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				results <- reserve(actor, 60)
			}()
		}
		close(start)
		workers.Wait()
		close(results)
		successes, rejected := 0, 0
		for err := range results {
			if err == nil {
				successes++
			} else if errors.Is(err, domain.ErrQuotaExceeded) {
				rejected++
			} else {
				t.Fatal("并发预占出现其他故障", err)
			}
		}
		if successes != 1 || rejected != 1 || readState(1).reserved != 60 {
			t.Fatal("并发预占超额或结果数量错误")
		}
	})
	t.Run("reservation object and task commit together", func(t *testing.T) {
		setUsage(100, 0, 0)
		create := func(key string, parentID uint64, afterCreate error) error {
			prepared, err := service.ValidateCreateUploadInput(domain.CreateUploadInput{ParentID: parentID, Name: "  学习笔记.MD  ", SizeBytes: 40})
			if err != nil {
				return err
			}
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				if err := txStore.ReserveUserUploadSpace(txContext, prepared.SizeBytes); err != nil {
					return err
				}
				object, err := txStore.CreateUserStagedObject(txContext, domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: key, SizeBytes: prepared.SizeBytes})
				if err != nil {
					return err
				}
				if _, err := txStore.CreateUserUploadTask(txContext, object.ID, *prepared, time.Now().Add(time.Hour)); err != nil {
					return err
				}
				return afterCreate
			})
		}
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		if err := create("reservations/committed", 11, nil); err != nil {
			t.Fatal(err)
		}
		var sum uint64
		if err := pool.QueryRowContext(ctx, "SELECT COALESCE(SUM(reserved_bytes),0) FROM upload_tasks WHERE user_id=1").Scan(&sum); err != nil || sum != 40 || readState(1).reserved != sum {
			t.Fatal("用户与任务预占未一起提交", err)
		}
		before := readState(1)
		failure := errors.New("after upload metadata failure")
		for _, fixture := range []struct {
			key       string
			parentID  uint64
			after     error
			wantError error
		}{
			{"reservations/bad-target", 999, nil, domain.ErrFileEntryNotFound},
			{"reservations/committed", 11, nil, domain.ErrStoredObjectLocationConflict},
			{"reservations/outer-failure", 11, failure, failure},
		} {
			if err := create(fixture.key, fixture.parentID, fixture.after); !errors.Is(err, fixture.wantError) || readState(1) != before {
				t.Fatal("后续上传失败未回滚用户预占", err)
			}
		}
		var tasks, objects int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM upload_tasks").Scan(&tasks); err != nil || tasks != 1 {
			t.Fatal("失败组合仍保留任务", err)
		}
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM stored_objects").Scan(&objects); err != nil || objects != 1 {
			t.Fatal("失败组合仍保留对象", err)
		}
	})

	// 4. 最大用户身份无需根目录关联即可预占，数值仍保留完整 uint64。
	t.Run("maximum identity and reservation", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash,storage_quota,reserved_storage) VALUES(?,'quota_max','完整整数用户','fixture_hash',?,?)", maximum, maximum, maximum-1); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'48ee758a-0727-42de-ae7d-3bb33155e148','完整整数设备','WINDOWS')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if err := reserve(domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}, 1); err != nil || readState(maximum).reserved != maximum {
			t.Fatal("最大用户身份或预占值错误", err)
		}
	})
}
