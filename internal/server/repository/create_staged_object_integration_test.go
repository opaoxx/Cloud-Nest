package repository

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestCreateUserStagedObjectIntegration(t *testing.T) {
	// 1. 在独占验证库中安装正式会话表，准备认证事务身份。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	sessionStart := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	sessionEnd := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if sessionStart < 0 || sessionEnd <= sessionStart {
		t.Fatal("正式会话表迁移边界错误")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[sessionStart:sessionEnd]); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'staged_owner','暂存用户','fixture_hash','ACTIVE'),(2,'staged_disabled','禁用用户','fixture_hash','DISABLED'),(3,'staged_other','另一用户','fixture_hash','ACTIVE')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','暂存设备','WEB'),(31,3,'38ee758a-0727-42de-ae7d-3bb33155e148','另一设备','WINDOWS')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),(37,3,31,UNHEX(REPEAT('33',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
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
	other := domain.Actor{UserID: 3, DeviceID: 31, SessionID: 37}
	inputFor := func(key string) domain.CreateStagedObjectInput {
		return domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: key, SizeBytes: 2048}
	}
	create := func(createContext context.Context, current domain.Actor, input domain.CreateStagedObjectInput) (*domain.StoredObject, error) {
		var prepared *domain.StoredObject
		err := store.WithUserTransaction(createContext, current, func(txContext context.Context, txStore *Store) error {
			var err error
			prepared, err = txStore.CreateUserStagedObject(txContext, input)
			return err
		})
		if err != nil {
			return nil, err
		}
		return prepared, nil
	}
	checkMissing := func(t *testing.T, key string) {
		t.Helper()
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM stored_objects WHERE provider='local_minio' AND bucket='cloudnest' AND object_key=?", key).Scan(&count); err != nil || count != 0 {
			t.Fatal("失败或回滚后仍有暂存对象", err)
		}
	}

	// 2. 正式写入、单连接回读与默认值验证。
	t.Run("one connection and zero-byte defaults", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		input := inputFor("staged/empty")
		input.SizeBytes = 0
		created, err := create(ctx, actor, input)
		if err != nil || created == nil || created.ID == 0 {
			t.Fatal("单连接暂存创建失败", err)
		}
		want := domain.StoredObject{ID: created.ID, OwnerUserID: 1, Provider: input.Provider, Bucket: input.Bucket,
			ObjectKey: input.ObjectKey, Status: domain.StoredObjectStatusStaging, CreatedAt: created.CreatedAt, UpdatedAt: created.UpdatedAt}
		if !reflect.DeepEqual(created, &want) || created.CreatedAt.IsZero() || created.CreatedAt != created.UpdatedAt || created.CreatedAt.Location() != time.UTC || created.CreatedAt.Nanosecond()%1000 != 0 {
			t.Fatal("创建记录的默认状态、空值或时间错误")
		}
		created.ObjectKey = "changed"
		again, err := store.FindUserStoredObject(ctx, 1, want.ID)
		if err != nil || !reflect.DeepEqual(again, &want) {
			t.Fatal("修改创建结果影响了已提交记录", err)
		}
	})
	t.Run("ownership and location conflicts", func(t *testing.T) {
		input := inputFor("staged/original")
		original, err := create(ctx, actor, input)
		if err != nil {
			t.Fatal(err)
		}
		for _, current := range []domain.Actor{actor, other} {
			created, err := create(ctx, current, input)
			if created != nil || !errors.Is(err, domain.ErrStoredObjectLocationConflict) || strings.Contains(err.Error(), input.ObjectKey) || strings.Contains(err.Error(), "Duplicate entry") {
				t.Fatal("重复位置未返回固定冲突错误", err)
			}
		}
		again, err := store.FindUserStoredObject(ctx, 1, original.ID)
		if err != nil || !reflect.DeepEqual(again, original) {
			t.Fatal("重复创建覆盖了原对象", err)
		}
		foreign, err := create(ctx, other, inputFor("staged/other"))
		if err != nil || foreign.OwnerUserID != other.UserID {
			t.Fatal("其他用户暂存创建归属错误", err)
		}
		if hidden, err := store.FindUserStoredObject(ctx, actor.UserID, foreign.ID); hidden != nil || !errors.Is(err, domain.ErrStoredObjectNotFound) {
			t.Fatal("跨用户读取未隔离", err)
		}
	})
	t.Run("callback and later conflict rollback", func(t *testing.T) {
		failure := errors.New("outer staged business failure")
		for _, phase := range []string{"callback", "conflict"} {
			key := "staged/rollback-" + phase
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				if _, err := txStore.CreateUserStagedObject(txContext, inputFor(key)); err != nil {
					return err
				}
				if phase == "conflict" {
					_, err := txStore.CreateUserStagedObject(txContext, inputFor("staged/original"))
					return err
				}
				return failure
			})
			wantError := failure
			if phase == "conflict" {
				wantError = domain.ErrStoredObjectLocationConflict
			}
			if !errors.Is(err, wantError) {
				t.Fatal("后续业务失败未保留错误", err)
			}
			checkMissing(t, key)
		}
	})
	t.Run("cancellation after insert rolls back", func(t *testing.T) {
		cancelled, stop := context.WithCancel(ctx)
		defer stop()
		key := "staged/cancelled"
		err := store.WithUserTransaction(cancelled, actor, func(txContext context.Context, txStore *Store) error {
			if _, err := txStore.CreateUserStagedObject(txContext, inputFor(key)); err != nil {
				return err
			}
			stop()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatal("创建后取消未阻止提交", err)
		}
		checkMissing(t, key)
	})
	t.Run("invalid input and identity scope", func(t *testing.T) {
		invalid := inputFor("staged/invalid")
		invalid.Provider = ""
		if created, err := create(ctx, actor, invalid); created != nil || !errors.Is(err, domain.ErrInvalidStoredObjectData) {
			t.Fatal("真实事务未拒绝无效创建参数", err)
		}
		for _, current := range []domain.Actor{{UserID: 2, DeviceID: 13, SessionID: 17}, {UserID: 1, DeviceID: 13, SessionID: 37}} {
			created, err := create(ctx, current, inputFor("staged/invalid-actor"))
			if created != nil || (!errors.Is(err, domain.ErrAccountDisabled) && !errors.Is(err, domain.ErrSessionNotFound)) {
				t.Fatal("禁用或错归属身份未阻止暂存创建", err)
			}
		}
		checkMissing(t, "staged/invalid")
		checkMissing(t, "staged/invalid-actor")
	})
	for _, fixture := range []struct {
		name, change, restore string
		wantError             error
	}{
		{"disabled account", "UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
		{"revoked device", "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=13", "UPDATE user_devices SET revoked_at=NULL WHERE id=13", domain.ErrDeviceRevoked},
		{"revoked session", "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		{"expired session", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=17", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=17", domain.ErrSessionExpired},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if _, err := pool.ExecContext(ctx, fixture.change); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := pool.ExecContext(ctx, fixture.restore); err != nil {
					t.Error(err)
				}
			}()
			key := "staged/" + fixture.name
			if created, err := create(ctx, actor, inputFor(key)); created != nil || !errors.Is(err, fixture.wantError) {
				t.Fatal("认证状态变化未阻止创建", err)
			}
			checkMissing(t, key)
		})
	}

	// 3. 两个用户并发竞争同一位置，唯一键确定一次成功和一次冲突。
	t.Run("concurrent location conflict", func(t *testing.T) {
		var workers sync.WaitGroup
		start := make(chan struct{})
		results := make(chan error, 2)
		for _, current := range []domain.Actor{actor, other} {
			workers.Add(1)
			go func(current domain.Actor) {
				defer workers.Done()
				<-start
				created, err := create(ctx, current, inputFor("staged/concurrent"))
				if err == nil && (created == nil || created.OwnerUserID != current.UserID) {
					err = fmt.Errorf("并发创建结果归属错误")
				}
				results <- err
			}(current)
		}
		close(start)
		workers.Wait()
		close(results)
		successes, conflicts := 0, 0
		for err := range results {
			if err == nil {
				successes++
			} else if errors.Is(err, domain.ErrStoredObjectLocationConflict) {
				conflicts++
			} else {
				t.Fatal("并发创建返回其他故障", err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatal("并发位置竞争的结果数量错误")
		}
	})

	// 4. 高位自增放在普通夹具之后，确认完整无符号身份、大小及回读。
	t.Run("unsigned owner size and generated object id", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'staged_max','完整整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'48ee758a-0727-42de-ae7d-3bb33155e148','完整整数设备','WINDOWS')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "ALTER TABLE stored_objects AUTO_INCREMENT = 9223372036854775810"); err != nil {
			t.Fatal(err)
		}
		input := inputFor("staged/maximum")
		input.SizeBytes = maximum
		created, err := create(ctx, domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}, input)
		if err != nil || created == nil || created.ID < uint64(1)<<63 || created.OwnerUserID != maximum || created.FileSize != maximum {
			t.Fatal("高位自增对象、完整身份或字节数丢失", err)
		}
	})
}
