package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartCancellationUnboundIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_CANCEL_UNBOUND_TEST") != "1" {
		t.Skip("显式启用后创建本轮专用 MySQL、MinIO 和合成数据")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	mysqlID := startIsolatedWebMySQL(t, ctx)
	t.Logf("专用 MySQL %s，回环地址 %s", mysqlID[:12], os.Getenv("CLOUDNEST_DB_ADDRESS"))
	minioID := startIsolatedWebMinIO(t, ctx)
	t.Logf("专用 MinIO %s，回环地址 %s", minioID[:12], os.Getenv("CLOUDNEST_S3_ENDPOINT"))
	bucket := prepareServerUploadContentBucket(t, ctx)
	pool, _ := prepareServerIntegrationDatabase(t, ctx)
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := config.LoadStorageConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := storage.NewClient(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	// 1. 建立独占账号及未绑定会话的合成取消任务。
	const userID, usedBytes = 7, 1024
	const partSize, fileSize = 16 << 20, (16 << 20) + 1
	const reservedBytes = 3 * fileSize
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(t, "INSERT INTO users(id,username,display_name,password_hash,used_storage,reserved_storage) VALUES(7,'unbound_cancel_fixture','未绑定会话取消','fixture_hash',?,?)", usedBytes, reservedBytes)
	execute(t, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute(t, "INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	core := minio.Core{Client: bucket.client}
	for _, fixture := range []struct {
		name, key      string
		taskID, object uint64
		withObject     bool
		lateBinding    bool
	}{
		{name: "missing object leaves orphan alone", key: "b4/unbound original +%.bin", taskID: 19, object: 23},
		{name: "real object version observed", key: "b4/unbound-object.bin", taskID: 20, object: 24, withObject: true},
		{name: "binding changes during HEAD", key: "b4/unbound-stale.bin", taskID: 21, object: 25, lateBinding: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			const payload = "synthetic unbound object"
			var existing minio.UploadInfo
			var unboundSession string
			if fixture.withObject {
				existing, err = bucket.client.PutObject(ctx, bucket.name, fixture.key, strings.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{})
				if err != nil || existing.VersionID == "" || existing.VersionID == "null" {
					t.Fatal("隔离桶未生成合成对象版本", minio.ToErrorResponse(err).Code)
				}
			} else {
				unboundSession, err = client.CreateMultipartUpload(ctx, fixture.key)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					if err := core.AbortMultipartUpload(cleanup, bucket.name, fixture.key, unboundSession); err != nil {
						t.Error("专用合成会话清理失败", minio.ToErrorResponse(err).Code)
					}
				})
			}
			execute(t, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,7,?,?,?,?)",
				fixture.object, settings.Provider, settings.Bucket, fixture.key, fileSize)
			execute(t, `INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
				expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
				VALUES(?,7,11,?,13,'fixture.bin','MULTIPART',NULL,UNHEX(REPEAT('ab',32)),?,?,2,'CANCELLING',?,UTC_TIMESTAMP(6)-INTERVAL 1 SECOND)`,
				fixture.taskID, fixture.object, fileSize, partSize, fileSize)
			candidates, err := store.ListMultipartUploadCancellationCandidates(ctx, 1)
			if err != nil || len(candidates) != 1 || candidates[0].TaskID != fixture.taskID {
				t.Fatal("未取得本项未绑定的取消候选", candidates, err)
			}
			claimed, err := store.ClaimMultipartUploadCancellationLease(ctx, candidates[0], "fixture-unbound-observer")
			if err != nil || claimed == nil || claimed.StorageUploadID != nil {
				t.Fatal("未领取空绑定任务的租约", err)
			}
			objectBefore, err := store.FindUserStoredObject(ctx, userID, fixture.object)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 使用正式仓储和 HEAD 适配器，在指定用例中于 HEAD 后模拟绑定变化。
			var heads int
			execution := service.NewMultipartUploadCancellationStorageService(
				func(callContext context.Context, task *domain.UploadTask) (*domain.MultipartUploadCancellationTarget, error) {
					return store.FindMultipartUploadCancellationTarget(callContext, task, settings.Provider, settings.Bucket)
				},
				func(context.Context, string, string) error {
					t.Fatal("未绑定任务发起了中止")
					return nil
				},
				func(context.Context, string, string) (*domain.MultipartUploadAbortObservation, error) {
					t.Fatal("未绑定任务查询了会话")
					return nil, nil
				},
				func(callContext context.Context, objectKey string) (*domain.UploadObjectResult, error) {
					heads++
					if objectKey != fixture.key {
						t.Fatal("HEAD 未使用原对象键")
					}
					object, err := client.StatMultipartUploadObject(callContext, objectKey)
					if fixture.lateBinding && errors.Is(err, domain.ErrUploadObjectMissing) {
						execute(t, "UPDATE upload_tasks SET storage_upload_id=? WHERE user_id=7 AND id=?", unboundSession, fixture.taskID)
					}
					return object, err
				})
			observed, err := execution.ObserveUnboundSession(ctx, claimed)
			if heads != 1 {
				t.Fatal("本次检查未恰好查询一次原对象", heads)
			}
			if fixture.lateBinding {
				if observed != nil || !errors.Is(err, domain.ErrUploadTaskBusy) {
					t.Fatal("绑定发生变化后仍返回了观察", err)
				}
			} else {
				if err != nil || observed == nil || observed.ObjectKey != fixture.key || !reflect.DeepEqual(observed.Task, *claimed) ||
					(observed.Object != nil) != fixture.withObject {
					t.Fatal("未返回匹配的空绑定观察", err)
				}
				if object := observed.Object; object != nil && (object.StorageVersionID == nil || *object.StorageVersionID != existing.VersionID ||
					object.StorageETag == nil || *object.StorageETag != existing.ETag || object.SizeBytes != uint64(len(payload))) {
					t.Fatal("原对象版本、ETag 或大小未保留")
				}
			}

			// 3. 独立确认孤儿会话仍存在，任务快照、对象和配额符合原记录。
			if unboundSession != "" {
				if _, err := core.ListObjectParts(ctx, bucket.name, fixture.key, unboundSession, 0, 1); err != nil {
					t.Fatal("同键未关联会话已不可读", minio.ToErrorResponse(err).Code)
				}
			}
			taskAfter, err := store.FindUserUploadTask(ctx, userID, fixture.taskID)
			want := *claimed
			if fixture.lateBinding {
				want.StorageUploadID = &unboundSession
			}
			if err != nil || !reflect.DeepEqual(taskAfter, &want) {
				t.Fatal("任务数据与预期不符", err)
			}
			objectAfter, err := store.FindUserStoredObject(ctx, userID, fixture.object)
			if err != nil || !reflect.DeepEqual(objectBefore, objectAfter) {
				t.Fatal("只读检查改变了暂存记录", err)
			}
			var used, reserved uint64
			if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&used, &reserved); err != nil || used != usedBytes || reserved != reservedBytes {
				t.Fatal("只读检查改变了配额", used, reserved, err)
			}
		})
	}
	t.Logf("对象缺失、版本存在、回读发现绑定变更均通过；未关联会话仍可读，已用 %d B、预占 %d B", usedBytes, reservedBytes)
}
