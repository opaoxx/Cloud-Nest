package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartUploadVerificationCandidateIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_VERIFICATION_CANDIDATE_TEST") != "1" {
		t.Skip("显式启用后创建专用 MySQL、MinIO 及合成对象")
	}
	// 1. 在独占实例准备待校验任务及保存了版本和 ETag 的合成对象。
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	mysqlContainer := startIsolatedWebMySQL(t, ctx)
	minioContainer := startIsolatedWebMinIO(t, ctx)
	t.Logf("专用 MySQL %s，MinIO %s，存储回环地址 %s", mysqlContainer[:12], minioContainer[:12], os.Getenv("CLOUDNEST_S3_ENDPOINT"))
	t.Setenv("CLOUDNEST_STORAGE_PROVIDER", "local_minio")
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

	const userID, partSize, fileSize = 7, 16 << 20, (16 << 20) + 1
	const worker, retryWorker = "fixture-candidate-worker", "fixture-retry-worker"
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	used, reserved := uint64(0), uint64(3*fileSize+512)
	execute(t, "INSERT INTO users(id,username,display_name,password_hash,reserved_storage) VALUES(7,'multipart_verify_fixture','分片校验','fixture_hash',?)", reserved)
	execute(t, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute(t, "INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	assertQuota := func(t *testing.T) {
		t.Helper()
		var actualUsed, actualReserved uint64
		if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=?", userID).Scan(&actualUsed, &actualReserved); err != nil {
			t.Fatal(err)
		}
		if actualUsed != used || actualReserved != reserved {
			t.Fatalf("账号账目不符，已用 %d，预占 %d", actualUsed, actualReserved)
		}
	}
	content := bytes.Repeat([]byte{0x22}, fileSize)
	checksum := sha256.Sum256(content)
	for _, scenario := range []struct {
		name, key, errorCode string
		taskID, objectID     uint64
		data                 []byte
		expectedDigest       [sha256.Size]byte
		wantError            error
	}{
		{name: "retry then publish original version", key: "b4/verify-publish.bin", taskID: 19, objectID: 23, data: content, expectedDigest: checksum},
		{name: "settle checksum mismatch", key: "b4/verify-checksum.bin", taskID: 20, objectID: 24, data: content,
			errorCode: domain.UploadChecksumMismatchCode, wantError: domain.ErrUploadChecksumMismatch},
		{name: "settle size mismatch", key: "b4/verify-size.bin", taskID: 21, objectID: 25, data: content[:2], expectedDigest: checksum,
			errorCode: domain.UploadBodySizeMismatchCode, wantError: domain.ErrUploadBodySizeMismatch},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			original, err := bucket.client.PutObject(ctx, bucket.name, scenario.key, bytes.NewReader(scenario.data), int64(len(scenario.data)), minio.PutObjectOptions{DisableMultipart: true})
			if err != nil || original.VersionID == "" || original.ETag == "" {
				t.Fatal("未获得独占桶中合成对象的版本与 ETag", minio.ToErrorResponse(err).Code)
			}
			shouldPublish := scenario.wantError == nil
			if shouldPublish {
				newer := []byte("new synthetic version")
				latest, err := bucket.client.PutObject(ctx, bucket.name, scenario.key, bytes.NewReader(newer), int64(len(newer)), minio.PutObjectOptions{DisableMultipart: true})
				if err != nil || latest.VersionID == "" || latest.VersionID == original.VersionID || latest.ETag == original.ETag {
					t.Fatal("同键的独立版本未写入独占桶", minio.ToErrorResponse(err).Code)
				}
			}
			execute(t, `INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,storage_version_id,storage_etag,file_size,status)
				VALUES(?,7,?,?,?,?,?,?,'VERIFYING')`, scenario.objectID, settings.Provider, bucket.name, scenario.key, original.VersionID, original.ETag, fileSize)
			execute(t, `INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
				expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
				VALUES(?,7,11,?,13,'verified.bin','MULTIPART','fixture-merged-session ',?,?,?,?, 'VERIFYING',?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`,
				scenario.taskID, scenario.objectID, scenario.expectedDigest[:], fileSize, partSize, 2, fileSize)
			readTask := func() *domain.UploadTask {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, userID, scenario.taskID)
				if err != nil {
					t.Fatal(err)
				}
				return task
			}
			readObject := func() *domain.StoredObject {
				t.Helper()
				object, err := store.FindUserStoredObject(ctx, userID, scenario.objectID)
				if err != nil {
					t.Fatal(err)
				}
				return object
			}
			scan := func() []domain.UploadVerificationCandidate {
				t.Helper()
				candidates, err := store.ListMultipartUploadVerificationCandidates(ctx, 1)
				if err != nil {
					t.Fatal(err)
				}
				return candidates
			}
			objectBefore := readObject()
			candidates := scan()
			if len(candidates) != 1 || candidates[0].TaskID != scenario.taskID {
				t.Fatal("未扫描到预期的 Multipart 候选", candidates)
			}

			// 2. 通过服务侧单条入口处理，只在第一次打开时注入临时故障。
			injectFault, activeWorker, openCount := shouldPublish, worker, 0
			candidate := candidates[0]
			processor := newServerMultipartUploadVerificationProcessor(store, func(openContext context.Context, object *domain.StoredObject) (io.ReadCloser, error) {
				openCount++
				claimed := readTask()
				if claimed.LeaseOwner == nil || *claimed.LeaseOwner != activeWorker || claimed.LeaseVersion != candidate.LeaseVersion+1 || claimed.LeaseUntil == nil ||
					!reflect.DeepEqual(object, objectBefore) {
					t.Fatal("打开对象前未提交正确租约，或未读取原对象定位", claimed, object)
				}
				if injectFault {
					return nil, domain.ErrUploadStorageUnavailable
				}
				return client.OpenUploadVerificationObject(openContext, object)
			})
			if injectFault {
				retry, err := processor.Process(ctx, candidate, activeWorker)
				if err != nil || retry == nil || !reflect.DeepEqual(retry, readTask()) || retry.Status != domain.UploadStatusVerifying ||
					retry.ReservedBytes != fileSize || retry.LastErrorCode == nil || *retry.LastErrorCode != "STORAGE_UNAVAILABLE" ||
					retry.LeaseOwner != nil || retry.LeaseUntil != nil || retry.AttemptCount != 1 || retry.NextAttemptAt == nil ||
					!retry.NextAttemptAt.Equal(retry.UpdatedAt.Add(time.Second)) || !reflect.DeepEqual(readObject(), objectBefore) || openCount != 1 {
					t.Fatal("临时故障没有保留任务与原对象并登记重试", retry, err)
				}
				assertQuota(t)
				// 将此夹具的重试时刻推进到已到点，下一次必须领取新版本。
				execute(t, "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6) WHERE id=?", scenario.taskID)
				due := scan()
				if len(due) != 1 || due[0].TaskID != candidate.TaskID || due[0].LeaseVersion != candidate.LeaseVersion+1 {
					t.Fatal("已安排的任务未以更新的租约版本再次入选", due)
				}
				candidate, activeWorker, injectFault = due[0], retryWorker, false
			}
			priorOpenCount := openCount
			result, err := processor.Process(ctx, candidate, activeWorker)
			if !errors.Is(err, scenario.wantError) || openCount != priorOpenCount+1 {
				t.Fatal("单条校验处理结果或对象读取次数错误", err, openCount)
			}

			// 3. 回读提交后的任务、对象与账目，确认重复候选停在读取前。
			saved, object := readTask(), readObject()
			if saved.LeaseVersion != candidate.LeaseVersion+1 || saved.LeaseOwner != nil || saved.LeaseUntil != nil || saved.ReservedBytes != 0 ||
				object.StorageVersionID == nil || *object.StorageVersionID != original.VersionID || object.StorageETag == nil || *object.StorageETag != original.ETag {
				t.Fatal("终态没有释放租约和预占，或改写了对象版本", saved, object)
			}
			reserved -= fileSize
			if shouldPublish {
				if !reflect.DeepEqual(result, saved) || saved.Status != domain.UploadStatusCompleted || saved.ResultEntryID == nil || saved.LastErrorCode != nil ||
					object.Status != domain.StoredObjectStatusReady || object.VerifiedSHA256 == nil || *object.VerifiedSHA256 != checksum {
					t.Fatal("成功校验未提交原版本和发布结果", saved, object)
				}
				var publishedObject, publishedSize uint64
				if err := pool.QueryRowContext(ctx, "SELECT object_id,file_size FROM file_entries WHERE id=? AND user_id=? AND entry_type='FILE'", *saved.ResultEntryID, userID).
					Scan(&publishedObject, &publishedSize); err != nil || publishedObject != object.ID || publishedSize != fileSize {
					t.Fatal("发布条目未指向已校验的对象", publishedObject, publishedSize, err)
				}
				used += fileSize
				t.Logf("任务 %d 以租约版本 %d 发布条目 %d，原版本 %s，大小 %d，SHA-256 %x", saved.ID, saved.LeaseVersion, *saved.ResultEntryID, original.VersionID, publishedSize, checksum)
			} else if result != nil || saved.Status != domain.UploadStatusFailed || saved.LastErrorCode == nil || *saved.LastErrorCode != scenario.errorCode ||
				saved.ResultEntryID != nil || saved.PublishedObjectID != nil || object.Status != domain.StoredObjectStatusOrphaned || object.GCAfter == nil {
				t.Fatal("已确认失配未写入预期终态和回收计时", saved, object)
			}
			assertQuota(t)
			if repeat, err := processor.Process(ctx, candidate, activeWorker); repeat != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) || openCount != priorOpenCount+1 {
				t.Fatal("同一候选在终态后重新读取对象", repeat, err, openCount)
			}
		})
	}
}
