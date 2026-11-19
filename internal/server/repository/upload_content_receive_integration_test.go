package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestUploadContentReceiveServiceIntegration(t *testing.T) {
	pool, _ := uploadTaskDatabase(t)
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表定义未找到")
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(migrations.InitialSQL[start:end])
	execute("INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)")
	execute("UPDATE stored_objects SET provider='local_minio', file_size=3 WHERE id=23")
	expected := sha256.Sum256([]byte("abc"))
	execute("UPDATE upload_tasks SET status='UPLOADING',upload_mode='SINGLE',storage_upload_id=NULL,part_size=NULL,part_count=NULL,published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,file_size=3,reserved_bytes=3,expected_sha256=?,lease_owner=NULL,lease_until=NULL,lease_version=0,expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 DAY WHERE id=19", expected[:])
	execute("UPDATE users SET reserved_storage=3 WHERE id=7")
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	preparer := service.NewUploadContentPreparationService("local_minio", "fixture-bucket", func(ctx context.Context, current domain.Actor, action func(context.Context, service.UploadContentPreparationTransaction) error) error {
		return store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
			return action(txContext, txStore)
		})
	})
	directory := t.TempDir()
	files, err := service.NewUploadContentFileService(directory, service.NewUploadTempSpace())
	if err != nil {
		t.Fatal(err)
	}
	tasks := service.NewUploadTaskService(store.FindUserUploadTask)
	lookup := func(ctx context.Context) *domain.UploadTask {
		t.Helper()
		task, err := tasks.Get(ctx, actor, 19)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	readObject := func(ctx context.Context, id uint64) *domain.StoredObject {
		t.Helper()
		object, err := store.FindUserStoredObject(ctx, actor.UserID, id)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}
	written := make(map[string]string)
	putObject := func(ctx context.Context, key string, file io.ReadSeeker, size uint64) (*domain.UploadObjectResult, error) {
		// 单连接也能读取已提交租约，证明准备事务在文件写入前已经结束。
		task := lookup(ctx)
		object := readObject(ctx, task.StagedObjectID)
		payload, err := io.ReadAll(file)
		if err != nil || task.LeaseOwner == nil || object.ObjectKey != key || object.Status != domain.StoredObjectStatusStaging || size != 3 || string(payload) != "abc" {
			t.Fatal("写入时未读到已提交的租约、对象或文件", err)
		}
		if _, exists := written[key]; exists {
			t.Fatal("重试复用了旧写入位置")
		}
		written[key] = string(payload)
		etag := "fixture-" + key
		return &domain.UploadObjectResult{SizeBytes: size, StorageETag: &etag}, nil
	}
	rollback := errors.New("fixture rollback after advance")
	failTransaction := true
	receiver := service.NewUploadContentReceiveService(files, "local_minio", "fixture-bucket", preparer.Prepare, putObject, func(ctx context.Context, current domain.Actor, action func(context.Context, service.UploadContentReceiveTransaction) error) error {
		return store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error {
			if err := action(txContext, txStore); err != nil {
				return err
			}
			if failTransaction {
				return rollback
			}
			return nil
		})
	})

	// 1. 写入成功后，模拟推进事务在提交前失败，已提交租约仍然有效。
	result, err := receiver.Receive(ctx, actor, lookup(ctx), strings.NewReader("abc"))
	if result != nil || !errors.Is(err, rollback) || len(written) != 1 {
		t.Fatal("未保留写入后的回滚原因", result, err)
	}
	task, original := lookup(ctx), readObject(ctx, 23)
	if task.Status != domain.UploadStatusUploading || task.LeaseVersion != 1 || task.LeaseOwner == nil ||
		original.Status != domain.StoredObjectStatusStaging || original.StorageETag != nil {
		t.Fatal("推进事务未整体回滚")
	}
	result, err = receiver.Receive(ctx, actor, task, strings.NewReader("abc"))
	if result != nil || !errors.Is(err, domain.ErrUploadTaskBusy) || len(written) != 1 {
		t.Fatal("有效租约未拒绝重入", err)
	}

	// 2. 令旧租约过期，再次接收生成新位置并提交 VERIFYING。
	execute("UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19")
	failTransaction = false
	result, err = receiver.Receive(ctx, actor, lookup(ctx), strings.NewReader("abc"))
	if err != nil || result == nil || result.Status != domain.UploadStatusVerifying || result.StagedObjectID == 23 ||
		result.LeaseVersion != 2 || result.LeaseOwner != nil || result.ReservedBytes != 3 || len(written) != 2 {
		t.Fatal("重试未推进校验", err)
	}
	task, current := lookup(ctx), readObject(ctx, result.StagedObjectID)
	if task.Status != result.Status || task.StagedObjectID != result.StagedObjectID ||
		current.Status != domain.StoredObjectStatusVerifying || current.StorageETag == nil || *current.StorageETag != "fixture-"+current.ObjectKey ||
		written[current.ObjectKey] != "abc" || written[original.ObjectKey] != "abc" || readObject(ctx, 23).Status != domain.StoredObjectStatusOrphaned {
		t.Fatal("未提交新尝试的双表结果和旧对象的孤立状态")
	}
	result, err = receiver.Receive(ctx, actor, task, strings.NewReader("abc"))
	if result != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) || len(written) != 2 {
		t.Fatal("校验阶段再次接收结果错误", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("接收后仍有临时文件", err)
	}
}
