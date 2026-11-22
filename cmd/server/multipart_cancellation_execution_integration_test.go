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

func TestServerMultipartCancellationExecutionIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_CANCEL_EXECUTION_TEST") != "1" {
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
	httpClient := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(httpClient.CloseIdleConnections)
	core := minio.Core{Client: bucket.client}

	// 1. 创建合成账号及隔离任务，用正式仓储扫描并领取取消租约。
	const userID, usedBytes = 7, 1024
	const partSize, fileSize = 16 << 20, (16 << 20) + 1
	var remainingReserved uint64 = 5*fileSize + 512
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(t, "INSERT INTO users(id,username,display_name,password_hash,used_storage,reserved_storage) VALUES(7,'cancel_executor_fixture','分片取消执行','fixture_hash',?,?)", usedBytes, remainingReserved)
	execute(t, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute(t, "INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	content := bytes.Repeat([]byte{0x42}, fileSize)
	digest := sha256.Sum256(content)
	for _, fixture := range []struct {
		name, key                    string
		taskID, objectID             uint64
		bound, withObject, untrusted bool
	}{
		{name: "bound missing", key: "b4/execute missing +%.bin", taskID: 19, objectID: 23, bound: true},
		{name: "bound existing", key: "b4/execute-merged.bin", taskID: 20, objectID: 24, bound: true, withObject: true},
		{name: "unbound missing with a separate session", key: "b4/execute-orphan.bin", taskID: 21, objectID: 25},
		{name: "unbound existing", key: "b4/execute-found.bin", taskID: 22, objectID: 26, withObject: true},
		{name: "pending until store is trusted", key: "b4/execute-pending.bin", taskID: 23, objectID: 27, bound: true, untrusted: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var session string
			var binding *string
			var existing minio.UploadInfo
			if fixture.bound || !fixture.withObject {
				var createError error
				session, createError = client.CreateMultipartUpload(ctx, fixture.key)
				if createError != nil {
					t.Fatal(createError)
				}
				t.Cleanup(func() {
					cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
					defer stop()
					if err := core.AbortMultipartUpload(cleanup, bucket.name, fixture.key, session); err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
						t.Error("专用会话清理失败", minio.ToErrorResponse(err).Code)
					}
				})
			}
			if fixture.bound {
				binding = &session
				probe := multipartProbe{core: core, bucket: bucket.name, key: fixture.key, uploadID: session, endpoint: settings.EndpointURL}
				payloads := [][]byte{content[partSize:]}
				firstNumber := 2
				if fixture.withObject {
					payloads, firstNumber = [][]byte{content[:partSize], content[partSize:]}, 1
				}
				parts := make([]minio.CompletePart, 0, len(payloads))
				for index, payload := range payloads {
					number := firstNumber + index
					checksum := multipartProbeSHA256(payload)
					signed := probe.sign(t, ctx, number, len(payload), checksum)
					etag := putMultipartProbePart(t, ctx, httpClient, signed, checksum, payload, http.StatusOK, "")
					parts = append(parts, minio.CompletePart{PartNumber: number, ETag: etag, ChecksumSHA256: checksum})
				}
				if fixture.withObject {
					var completeError error
					existing, completeError = core.CompleteMultipartUpload(ctx, bucket.name, fixture.key, session, parts, minio.PutObjectOptions{})
					if completeError != nil {
						t.Fatal("合成分片合并失败", minio.ToErrorResponse(completeError).Code)
					}
				}
			} else if fixture.withObject {
				payload := []byte("synthetic previous object")
				var putError error
				existing, putError = bucket.client.PutObject(ctx, bucket.name, fixture.key, bytes.NewReader(payload), int64(len(payload)), minio.PutObjectOptions{})
				if putError != nil {
					t.Fatal("写入隔离版本失败", minio.ToErrorResponse(putError).Code)
				}
			}
			if fixture.withObject && (existing.VersionID == "" || existing.VersionID == "null") {
				t.Fatal("隔离桶未产生确切版本")
			}
			execute(t, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,7,?,?,?,?)",
				fixture.objectID, settings.Provider, settings.Bucket, fixture.key, fileSize)
			execute(t, `INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
				expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
				VALUES(?,7,11,?,13,'fixture.bin','MULTIPART',?,?,?,?,2,'CANCELLING',?,UTC_TIMESTAMP(6)-INTERVAL 1 SECOND)`,
				fixture.taskID, fixture.objectID, binding, digest[:], fileSize, partSize, fileSize)
			candidates, err := store.ListMultipartUploadCancellationCandidates(ctx, 1)
			if err != nil || len(candidates) != 1 || candidates[0].TaskID != fixture.taskID {
				t.Fatal("未定位本项取消候选", candidates, err)
			}
			task, err := store.ClaimMultipartUploadCancellationLease(ctx, candidates[0], "fixture-cancel-execution")
			if err != nil {
				t.Fatal(err)
			}

			// 2. 串联正式存储观察和终态事务，选择本项的确认路径。
			verify := isolatedMinIOVerifier(minioID)
			if fixture.untrusted {
				verify = nil
			}
			observations := service.NewMultipartUploadCancellationStorageService(
				func(callContext context.Context, task *domain.UploadTask) (*domain.MultipartUploadCancellationTarget, error) {
					return store.FindMultipartUploadCancellationTarget(callContext, task, settings.Provider, settings.Bucket)
				}, client.AbortMultipartUpload,
				func(callContext context.Context, key, session string) (*domain.MultipartUploadAbortObservation, error) {
					return client.CheckMultipartUploadAfterAbort(callContext, key, session, verify)
				}, client.StatMultipartUploadObject)
			execution := service.NewMultipartUploadCancellationExecutionService(settings.Provider, settings.Bucket,
				observations.ObserveBoundSession, observations.ObserveUnboundSession,
				store.CompleteMissingMultipartUploadCancellation, store.CompleteExistingMultipartUploadCancellation)
			finished, err := execution.Execute(ctx, task)
			if err != nil || (finished == nil) != fixture.untrusted {
				t.Fatal("确定或待确认的执行结果不符", err)
			}

			// 3. 读回任务、版本和配额，核对存储中的准确对象。
			current, err := store.FindUserUploadTask(ctx, userID, fixture.taskID)
			if err != nil {
				t.Fatal(err)
			}
			staged, err := store.FindUserStoredObject(ctx, userID, fixture.objectID)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.untrusted {
				if !reflect.DeepEqual(current, task) || staged.Status != domain.StoredObjectStatusStaging || staged.GCAfter != nil {
					t.Fatal("未确认时更改了取消任务或对象")
				}
			} else {
				remainingReserved -= fileSize
				if !reflect.DeepEqual(current, finished) || current.Status != domain.UploadStatusCancelled || current.ReservedBytes != 0 ||
					staged.Status != domain.StoredObjectStatusOrphaned || staged.UnreferencedAt == nil || staged.GCAfter == nil ||
					!staged.GCAfter.Equal(staged.UnreferencedAt.Add(7*24*time.Hour)) {
					t.Fatal("终态、预占或回收登记未随执行提交")
				}
			}
			var used, reserved uint64
			if err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage FROM users WHERE id=7").Scan(&used, &reserved); err != nil ||
				used != usedBytes || reserved != remainingReserved {
				t.Fatal("账号结算数值不符", used, reserved, err)
			}
			if fixture.withObject {
				stored, err := client.StatMultipartUploadObject(ctx, fixture.key)
				if err != nil || stored == nil || stored.StorageVersionID == nil || *stored.StorageVersionID != existing.VersionID ||
					staged.StorageVersionID == nil || *staged.StorageVersionID != existing.VersionID ||
					staged.StorageETag == nil || *staged.StorageETag != existing.ETag {
					t.Fatal("对象版本或回收定位未保留", err)
				}
			} else {
				_, err := client.ListUploadPartsPage(ctx, fixture.key, session, 0, 1)
				if fixture.bound && !errors.Is(err, domain.ErrUploadPartSessionMissing) || !fixture.bound && err != nil {
					t.Fatal("会话中止或未关联会话保留结果不符", err)
				}
			}
		})
	}
	t.Logf("四条确认路径已提交 CANCELLED，一条待确认保留租约与预占。已用 %d B、预占 %d B", usedBytes, remainingReserved)
}
