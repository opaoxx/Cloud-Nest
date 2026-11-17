package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestCreateUserUploadTaskIntegration(t *testing.T) {
	// 1. 安装独占库中的正式会话与任务表，准备目标和身份。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	for _, boundary := range []struct{ table, next string }{{"auth_sessions", "stored_objects"}, {"upload_tasks", "idempotency_records"}} {
		start := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.table+" (")
		end := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.next+" (")
		if start < 0 || end <= start {
			t.Fatal("正式表迁移边界错误")
		}
		if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'task_owner','上传用户','fixture_hash','ACTIVE'),(2,'task_disabled','禁用用户','fixture_hash','DISABLED'),(3,'task_other','另一用户','fixture_hash','ACTIVE')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','上传设备','WEB'),(31,3,'38ee758a-0727-42de-ae7d-3bb33155e148','另一设备','WINDOWS')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),(37,3,31,UNHEX(REPEAT('33',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'local_minio','cloudnest','task/file',1),(43,3,'local_minio','cloudnest','task/foreign',2048),(45,1,'local_minio','cloudnest','task/unused',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(12,1,11,'file.bin',X'66696c652e62696e','FILE','OTHER',41,1)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(15,1,11,'trash',X'7472617368','DIR','TRASHED',UTC_TIMESTAMP(6))",
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
	expiry := time.Now().UTC().Truncate(time.Microsecond).Add(24 * time.Hour)
	inputFor := func() domain.CreateUploadInput {
		return domain.CreateUploadInput{ParentID: 11, Name: "学习笔记.MD", SizeBytes: 2048}
	}
	create := func(createContext context.Context, current domain.Actor, input domain.CreateUploadInput, key string, expiresAt time.Time) (*domain.UploadTask, error) {
		prepared, err := service.ValidateCreateUploadInput(input)
		if err != nil {
			return nil, err
		}
		var task *domain.UploadTask
		err = store.WithUserTransaction(createContext, current, func(txContext context.Context, txStore *Store) error {
			object, err := txStore.CreateUserStagedObject(txContext, domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: key, SizeBytes: prepared.SizeBytes})
			if err != nil {
				return err
			}
			task, err = txStore.CreateUserUploadTask(txContext, object.ID, *prepared, expiresAt)
			return err
		})
		if err != nil {
			return nil, err
		}
		return task, nil
	}
	checkMissing := func(t *testing.T, key string) {
		t.Helper()
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM stored_objects WHERE provider='local_minio' AND bucket='cloudnest' AND object_key=?", key).Scan(&count); err != nil || count != 0 {
			t.Fatal("失败事务仍保留暂存对象", err)
		}
	}

	// 2. 声明校验、对象创建和任务创建在单连接同一事务完成。
	t.Run("one connection declarations and defaults", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		for _, mode := range []string{"empty", "ordinary", "digest", "zero digest", "single limit"} {
			input := inputFor()
			switch mode {
			case "empty":
				input.SizeBytes = 0
			case "digest":
				digest := sha256.Sum256([]byte("content"))
				input.ExpectedSHA256 = &digest
			case "zero digest":
				input.ExpectedSHA256 = new([sha256.Size]byte)
			case "single limit":
				input.SizeBytes = domain.MaxSingleUploadSizeBytes
			}
			input.Name = "  学习笔记.MD  "
			deadline := expiry.In(time.FixedZone("fixture", 8*3600)).Add(789 * time.Nanosecond)
			task, err := create(ctx, actor, input, "tasks/"+mode, deadline)
			if err != nil || task == nil || task.ID == 0 || task.UserID != 1 || task.DeviceID != 13 || task.StagedObjectID == 0 ||
				task.TargetParentID != 11 || task.RequestedName != "学习笔记.MD" || task.UploadMode != "SINGLE" || task.Status != domain.UploadStatusUploading ||
				task.FileSize != input.SizeBytes || task.ReservedBytes != input.SizeBytes || !task.ExpiresAt.Equal(expiry) || !reflect.DeepEqual(task.ExpectedSHA256, input.ExpectedSHA256) {
				t.Fatal("完整上传任务创建结果错误", err)
			}
			again, err := store.FindUserUploadTask(ctx, 1, task.ID)
			if err != nil || !reflect.DeepEqual(again, task) {
				t.Fatal("提交后任务结果不一致", err)
			}
			task.RequestedName = "changed"
			if task.ExpectedSHA256 != nil {
				task.ExpectedSHA256[0] ^= 0xff
			}
			fresh, err := store.FindUserUploadTask(ctx, 1, task.ID)
			if err != nil || !reflect.DeepEqual(fresh, again) {
				t.Fatal("修改任务副本影响数据库", err)
			}
			if foreign, err := store.FindUserUploadTask(ctx, 3, task.ID); foreign != nil || !errors.Is(err, domain.ErrUploadTaskNotFound) {
				t.Fatal("任务跨用户范围未隔离", err)
			}
		}
	})
	t.Run("target failures roll back new object", func(t *testing.T) {
		for index, parentID := range []uint64{999, 33, 12, 15} {
			input := inputFor()
			input.ParentID = parentID
			key := "tasks/bad-target-" + string(rune('a'+index))
			task, err := create(ctx, actor, input, key, expiry)
			wantError := domain.ErrFileEntryNotFound
			if parentID == 12 {
				wantError = domain.ErrFileEntryNotDirectory
			}
			if task != nil || !errors.Is(err, wantError) {
				t.Fatal("无效目标未拒绝", err)
			}
			checkMissing(t, key)
		}
	})
	t.Run("staged ownership size and state", func(t *testing.T) {
		for _, fixture := range []struct {
			objectID uint64
			size     uint64
			wantErr  error
		}{
			{43, 2048, domain.ErrStoredObjectNotFound}, {999, 2048, domain.ErrStoredObjectNotFound}, {45, 1, domain.ErrInvalidUploadTaskData},
		} {
			input := inputFor()
			input.SizeBytes = fixture.size
			err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
				_, err := txStore.CreateUserUploadTask(txContext, fixture.objectID, input, expiry)
				return err
			})
			if !errors.Is(err, fixture.wantErr) {
				t.Fatal("无效暂存引用未拒绝", err)
			}
		}
		if _, err := pool.ExecContext(ctx, "UPDATE stored_objects SET status='ORPHANED' WHERE id=45"); err != nil {
			t.Fatal(err)
		}
		err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			_, err := txStore.CreateUserUploadTask(txContext, 45, inputFor(), expiry)
			return err
		})
		if !errors.Is(err, domain.ErrInvalidUploadTaskData) {
			t.Fatal("非初始暂存状态未拒绝", err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE stored_objects SET status='STAGING' WHERE id=45"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("outer failure and cancellation roll back both records", func(t *testing.T) {
		failure := errors.New("outer upload creation failure")
		for _, mode := range []string{"callback", "cancelled", "expired"} {
			key := "tasks/rollback-" + mode
			createContext, stop := context.WithCancel(ctx)
			deadline := expiry
			if mode == "expired" {
				deadline = time.Now().Add(-time.Second)
			}
			err := store.WithUserTransaction(createContext, actor, func(txContext context.Context, txStore *Store) error {
				object, err := txStore.CreateUserStagedObject(txContext, domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: key, SizeBytes: 2048})
				if err != nil {
					return err
				}
				if _, err := txStore.CreateUserUploadTask(txContext, object.ID, inputFor(), deadline); err != nil {
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
			if mode == "expired" {
				wantError = domain.ErrInvalidUploadTaskData
			}
			if !errors.Is(err, wantError) {
				t.Fatal("对象与任务组合回滚原因错误", err)
			}
			checkMissing(t, key)
		}
	})

	// 3. 真实认证状态变化阻止对象及任务写入。
	for _, fixture := range []struct {
		name, change, restore string
		wantError             error
	}{
		{"disabled", "UPDATE users SET status='DISABLED' WHERE id=1", "UPDATE users SET status='ACTIVE' WHERE id=1", domain.ErrAccountDisabled},
		{"device revoked", "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=13", "UPDATE user_devices SET revoked_at=NULL WHERE id=13", domain.ErrDeviceRevoked},
		{"session revoked", "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", domain.ErrSessionRevoked},
		{"session expired", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=17", "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=17", domain.ErrSessionExpired},
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
			key := "tasks/" + fixture.name
			if task, err := create(ctx, actor, inputFor(), key, expiry); task != nil || !errors.Is(err, fixture.wantError) {
				t.Fatal("认证状态未阻止任务创建", err)
			}
			checkMissing(t, key)
		})
	}
	t.Run("concurrent staged reference conflict", func(t *testing.T) {
		var workers sync.WaitGroup
		start := make(chan struct{})
		results := make(chan error, 2)
		for range 2 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				results <- store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
					_, err := txStore.CreateUserUploadTask(txContext, 45, inputFor(), expiry)
					return err
				})
			}()
		}
		close(start)
		workers.Wait()
		close(results)
		successes, conflicts := 0, 0
		for err := range results {
			if err == nil {
				successes++
			} else if errors.Is(err, domain.ErrUploadTaskStagedObjectConflict) {
				conflicts++
				if strings.Contains(err.Error(), "Duplicate entry") {
					t.Fatal("重复引用提示包含数据库原文")
				}
			} else {
				t.Fatal("并发任务创建出现其他故障", err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatal("同一暂存对象并发创建结果错误")
		}
	})

	// 4. 高位身份、对象及任务自增值放在普通验证之后。
	t.Run("unsigned identities and generated references", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'task_max','完整整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'48ee758a-0727-42de-ae7d-3bb33155e148','完整整数设备','WINDOWS')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"ALTER TABLE stored_objects AUTO_INCREMENT = 9223372036854775810", "ALTER TABLE upload_tasks AUTO_INCREMENT = 9223372036854775810"} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		input := domain.CreateUploadInput{ParentID: maximum, Name: strings.Repeat("a", 120), SizeBytes: domain.MaxSingleUploadSizeBytes}
		task, err := create(ctx, domain.Actor{UserID: maximum, DeviceID: maximum, SessionID: maximum}, input, "tasks/maximum", time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC))
		if err != nil || task == nil || task.ID < uint64(1)<<63 || task.StagedObjectID < uint64(1)<<63 || task.UserID != maximum || task.DeviceID != maximum || task.TargetParentID != maximum || task.ReservedBytes != domain.MaxSingleUploadSizeBytes {
			t.Fatal("高位身份或生成标识未完整回读", err)
		}
	})
}
