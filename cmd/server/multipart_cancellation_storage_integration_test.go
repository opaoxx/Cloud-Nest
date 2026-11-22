package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartCancellationStorageIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_CANCEL_STORAGE_TEST") != "1" {
		t.Skip("显式启用后创建本轮专用 MySQL、MinIO 和合成数据")
	}
	verifyServerMultipartBoundCleanupStorage(t, domain.UploadStatusCancelling)
}

func TestServerMultipartExpirationStorageIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_EXPIRATION_STORAGE_TEST") != "1" {
		t.Skip("显式启用后验证专用桶中的分片到期存储复查")
	}
	verifyServerMultipartBoundCleanupStorage(t, domain.UploadStatusExpiring)
}

func verifyServerMultipartBoundCleanupStorage(t *testing.T, pendingStatus domain.UploadStatus) {
	t.Helper()
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
	httpClient := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(httpClient.CloseIdleConnections)
	core := minio.Core{Client: bucket.client}

	// 1. 创建合成账号、暂存对象及待处理的分片任务，用正式仓储领取租约。
	const userID, usedBytes = 7, 1024
	const partSize, fileSize = 16 << 20, (16 << 20) + 1
	const reservedBytes = 4 * fileSize
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	execute(t, "INSERT INTO users(id,username,display_name,password_hash,used_storage,reserved_storage) VALUES(7,'multipart_cancel_fixture','分片取消','fixture_hash',?,?)", usedBytes, reservedBytes)
	execute(t, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute(t, "INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	content := bytes.Repeat([]byte{0x51}, fileSize)
	expectedSHA256 := sha256.Sum256(content)
	for _, fixture := range []struct {
		name, key              string
		taskID, stagedObjectID uint64
		completed, untrusted   bool
		replaceLease           bool
	}{
		{name: "aborts original session", key: "b4/cancel original +%.bin", taskID: 19, stagedObjectID: 23},
		{name: "retains version already completed", key: "b4/cancel-after-complete.bin", taskID: 20, stagedObjectID: 24, completed: true},
		{name: "untrusted observation remains pending", key: "b4/cancel-pending.bin", taskID: 21, stagedObjectID: 25, untrusted: true},
		{name: "loses lease after inspection", key: "b4/cancel-stale.bin", taskID: 22, stagedObjectID: 26, replaceLease: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			uploadID, err := client.CreateMultipartUpload(ctx, fixture.key)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if err := core.AbortMultipartUpload(cleanup, bucket.name, fixture.key, uploadID); err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
					t.Error("隔离会话清理失败", minio.ToErrorResponse(err).Code)
				}
			})
			probe := multipartProbe{core: core, bucket: bucket.name, key: fixture.key, uploadID: uploadID, endpoint: settings.EndpointURL}
			put := func(number int, payload []byte) minio.CompletePart {
				t.Helper()
				checksum := multipartProbeSHA256(payload)
				signed := probe.sign(t, ctx, number, len(payload), checksum)
				etag := putMultipartProbePart(t, ctx, httpClient, signed, checksum, payload, http.StatusOK, "")
				return minio.CompletePart{PartNumber: number, ETag: etag, ChecksumSHA256: checksum}
			}
			tail := put(2, content[partSize:])
			var existing minio.UploadInfo
			if fixture.completed {
				parts := []minio.CompletePart{put(1, content[:partSize]), tail}
				existing, err = core.CompleteMultipartUpload(ctx, bucket.name, fixture.key, uploadID, parts, minio.PutObjectOptions{})
				if err != nil || existing.VersionID == "" || existing.VersionID == "null" {
					t.Fatal("未生成合成对象版本", minio.ToErrorResponse(err).Code)
				}
			}
			execute(t, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,7,?,?,?,?)",
				fixture.stagedObjectID, settings.Provider, bucket.name, fixture.key, fileSize)
			initialStatus := pendingStatus
			if pendingStatus == domain.UploadStatusExpiring {
				initialStatus = domain.UploadStatusCompleting
			}
			execute(t, `INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
				expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
				VALUES(?,7,11,?,13,'fixture.bin','MULTIPART',?,?,?,?,2,?,?,UTC_TIMESTAMP(6)-INTERVAL 1 SECOND)`,
				fixture.taskID, fixture.stagedObjectID, uploadID, expectedSHA256[:], fileSize, partSize, initialStatus, fileSize)
			var claimed *domain.UploadTask
			if pendingStatus == domain.UploadStatusExpiring {
				candidates, scanError := store.ListMultipartUploadExpirationCandidates(ctx, 1)
				if scanError != nil || len(candidates) != 1 || candidates[0].TaskID != fixture.taskID {
					t.Fatal("没有获取本项到期候选", candidates, scanError)
				}
				claimed, err = store.ClaimMultipartUploadExpirationLease(ctx, candidates[0], "fixture-expiration-observer")
			} else {
				candidates, scanError := store.ListMultipartUploadCancellationCandidates(ctx, 1)
				if scanError != nil || len(candidates) != 1 || candidates[0].TaskID != fixture.taskID {
					t.Fatal("没有获取本项取消候选", candidates, scanError)
				}
				claimed, err = store.ClaimMultipartUploadCancellationLease(ctx, candidates[0], "fixture-cancel-observer")
			}
			if err != nil {
				t.Fatal(err)
			}
			objectBefore, err := store.FindUserStoredObject(ctx, userID, fixture.stagedObjectID)
			if err != nil {
				t.Fatal(err)
			}

			// 2. 用同一配置串联正式查询、中止和复查，在指定用例中模拟租约被替换。
			verify := isolatedMinIOVerifier(minioID)
			if fixture.untrusted {
				verify = nil
			}
			var aborts int
			abort := func(callContext context.Context, objectKey, uploadID string) error {
				aborts++
				return client.AbortMultipartUpload(callContext, objectKey, uploadID)
			}
			check := func(callContext context.Context, objectKey, uploadID string) (*domain.MultipartUploadAbortObservation, error) {
				observed, err := client.CheckMultipartUploadAfterAbort(callContext, objectKey, uploadID, verify)
				if err == nil && observed != nil && fixture.replaceLease {
					execute(t, "UPDATE upload_tasks SET lease_version=lease_version+1 WHERE user_id=7 AND id=?", fixture.taskID)
				}
				return observed, err
			}
			var observe func(context.Context, *domain.UploadTask) (*domain.MultipartUploadCleanupObservation, error)
			if pendingStatus == domain.UploadStatusExpiring {
				observe = service.NewMultipartUploadExpirationStorageService(
					func(callContext context.Context, task *domain.UploadTask) (*domain.MultipartUploadCleanupTarget, error) {
						return store.FindMultipartUploadExpirationTarget(callContext, task, settings.Provider, settings.Bucket)
					}, abort, check).ObserveBoundSession
			} else {
				observe = service.NewMultipartUploadCancellationStorageService(
					func(callContext context.Context, task *domain.UploadTask) (*domain.MultipartUploadCancellationTarget, error) {
						return store.FindMultipartUploadCancellationTarget(callContext, task, settings.Provider, settings.Bucket)
					}, abort, check, client.StatMultipartUploadObject).ObserveBoundSession
			}
			observed, err := observe(ctx, claimed)
			if aborts != 1 {
				t.Fatal("单次执行的中止请求次数错误", aborts)
			}
			if fixture.replaceLease {
				if observed != nil || !errors.Is(err, domain.ErrUploadTaskBusy) {
					t.Fatal("租约被替换后仍交付了存储观察", err)
				}
			} else if err != nil || (observed == nil) != fixture.untrusted {
				t.Fatal("存储已确认或待确认的结果不符", err)
			}
			if observed != nil {
				if observed.ObjectKey != fixture.key || !reflect.DeepEqual(observed.Task, *claimed) || (observed.Object != nil) != fixture.completed {
					t.Fatal("未保留原租约、存储目标或观察种类")
				}
				if object := observed.Object; object != nil && (object.StorageVersionID == nil || *object.StorageVersionID != existing.VersionID ||
					object.StorageETag == nil || *object.StorageETag != existing.ETag || object.SizeBytes != fileSize) {
					t.Fatal("已生成对象的版本、ETag 或大小不符")
				}
			}

			// 3. 独立确认原会话缺失及数据库保持原任务、对象和账目。
			if _, err := client.ListUploadPartsPage(ctx, fixture.key, uploadID, 0, 1); !errors.Is(err, domain.ErrUploadPartSessionMissing) {
				t.Fatal("原合成会话仍可读取或结果不明", err)
			}
			if fixture.completed {
				object, err := bucket.client.StatObject(ctx, bucket.name, fixture.key, minio.StatObjectOptions{VersionID: existing.VersionID})
				if err != nil || object.VersionID != existing.VersionID || object.ETag != existing.ETag || object.Size != fileSize {
					t.Fatal("原合成对象版本未保留", err)
				}
			}
			taskAfter, err := store.FindUserUploadTask(ctx, userID, fixture.taskID)
			want := *claimed
			if fixture.replaceLease {
				want.LeaseVersion++
			}
			if err != nil || !reflect.DeepEqual(taskAfter, &want) {
				t.Fatal("存储侧收尾改变了任务记录", err)
			}
			objectAfter, err := store.FindUserStoredObject(ctx, userID, fixture.stagedObjectID)
			if err != nil || !reflect.DeepEqual(objectBefore, objectAfter) {
				t.Fatal("存储侧收尾改变了暂存记录", err)
			}
			var used, reserved uint64
			if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&used, &reserved); err != nil || used != usedBytes || reserved != reservedBytes {
				t.Fatal("存储侧收尾改变了配额", used, reserved, err)
			}
		})
	}
	t.Logf("%s 四种合成场景各中止一次，账目保持已用 %d B、预占 %d B", pendingStatus, usedBytes, reservedBytes)
}
