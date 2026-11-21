package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartMergeExecutionIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_MERGE_EXECUTION_TEST") != "1" {
		t.Skip("显式启用后创建本轮专用 MySQL、MinIO 和合成数据")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	mysqlContainer := startIsolatedWebMySQL(t, ctx)
	minioContainer := startIsolatedWebMinIO(t, ctx)
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
	t.Logf("专用 MySQL %s，MinIO %s，存储回环地址 %s", mysqlContainer[:12], minioContainer[:12], settings.EndpointURL)
	client, err := storage.NewClient(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	const partSize = 16 << 20
	const fileSize = partSize + 1
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 41}
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	execute(t, "INSERT INTO users(id,username,display_name,password_hash,reserved_storage) VALUES(7,'multipart_merge_fixture','分片合并','fixture_hash',?)", 2*fileSize)
	execute(t, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute(t, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(41,7,11,UNHEX(REPEAT('41',32)),UTC_TIMESTAMP(6)+INTERVAL 1 DAY)")
	execute(t, "INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	targets := service.NewUploadPartSigningTargetService(settings.Provider, settings.Bucket, store.FindUserUploadTaskForPartSigning, store.FindUserStoredObject)
	recovery := service.NewUploadMergeRecoveryService(targets.GetForMerge, client.StatMultipartUploadObject)
	receiptCheck := service.NewUploadMergeReceiptService(targets.GetForMerge, store.ReadMultipartCompletionReceipts,
		func(callContext context.Context, key, uploadID string) ([]domain.UploadPartReceipt, error) {
			return client.ListUploadParts(callContext, key, uploadID, 1)
		})
	preflight := service.NewUploadMergePreflightService(recovery, receiptCheck)
	core := minio.Core{Client: bucket.client}
	transport := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(transport.CloseIdleConnections)
	first, tail := bytes.Repeat([]byte{0x22}, partSize), []byte{0x33}
	digest := sha256.New()
	digest.Write(first)
	digest.Write(tail)
	expectedDigest := digest.Sum(nil)

	for _, fixture := range []struct {
		name, key        string
		taskID, objectID uint64
		loseResponse     bool
	}{
		{"new completion", "b4/execution-normal.bin", 19, 23, false},
		{"recover confirmed version after lost response", "b4/execution-recovered.bin", 20, 24, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 合成分片走正式回执冻结，再领取本轮合并租约。
			uploadID, err := client.CreateMultipartUpload(ctx, fixture.key)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
				defer stop()
				if err := core.AbortMultipartUpload(cleanup, bucket.name, fixture.key, uploadID); err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
					t.Error("清理专用会话失败", minio.ToErrorResponse(err).Code)
				}
			})
			probe := multipartProbe{core: core, bucket: bucket.name, key: fixture.key, uploadID: uploadID, endpoint: settings.EndpointURL}
			for index, data := range [][]byte{first, tail} {
				checksum := multipartProbeSHA256(data)
				putMultipartProbePart(t, ctx, transport, probe.sign(t, ctx, index+1, len(data), checksum), checksum, data, http.StatusOK, "")
			}
			parts, err := client.ListUploadParts(ctx, fixture.key, uploadID, 1)
			if err != nil || len(parts) != 2 {
				t.Fatal("未取得两个合成分片回执", err)
			}
			execute(t, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,7,?,?,?,?)",
				fixture.objectID, settings.Provider, bucket.name, fixture.key, fileSize)
			execute(t, `INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
				expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
				VALUES(?,7,11,?,13,'fixture.bin','MULTIPART',?,?,?,16777216,2,'UPLOADING',?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`,
				fixture.taskID, fixture.objectID, uploadID, expectedDigest, fileSize, fileSize)
			readTask := func() *domain.UploadTask {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, actor.UserID, fixture.taskID)
				if err != nil {
					t.Fatal(err)
				}
				return task
			}
			requestLease, err := store.ClaimUserMultipartCompletionLease(ctx, actor, readTask(), "fixture-check", service.ResolveMultipartUploadCompletionStatus)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CommitUserMultipartCompletion(ctx, actor, requestLease, parts); err != nil {
				t.Fatal(err)
			}
			claim := func(owner string) *domain.UploadTask {
				t.Helper()
				candidates, err := store.ListMultipartUploadCompletionCandidates(ctx, 1)
				if err != nil || len(candidates) != 1 || candidates[0].TaskID != fixture.taskID {
					t.Fatal("合并候选不符", candidates, err)
				}
				lease, err := store.ClaimMultipartUploadCompletionLease(ctx, candidates[0], owner)
				if err != nil {
					t.Fatal(err)
				}
				return lease
			}
			lease := claim("fixture-merge")

			// 2. 真实完成后可模拟响应丢失，复验新租约应从 HEAD 恢复而跳过合并调用。
			completeCalls := 0
			execution := service.NewUploadMergeExecutionService(settings.Provider, settings.Bucket, preflight.Check,
				func(callContext context.Context, key, uploadID string, size, partSize uint64, count uint32, receipts []domain.UploadPartReceipt) (*domain.UploadObjectResult, error) {
					completeCalls++
					confirmation, err := client.CompleteMultipartUpload(callContext, key, uploadID, size, partSize, count, receipts)
					if err == nil && fixture.loseResponse {
						return nil, domain.ErrMultipartUploadCompletionUnknown
					}
					return confirmation, err
				}, store.BeginMultipartUploadVerification)
			result, err := execution.Execute(ctx, lease)
			if fixture.loseResponse {
				if result != nil || !errors.Is(err, domain.ErrMultipartUploadCompletionUnknown) || readTask().Status != domain.UploadStatusCompleting || completeCalls != 1 {
					t.Fatal("结果未确认的首轮不应推进或重放", result, err, completeCalls)
				}
				object, readError := store.FindUserStoredObject(ctx, actor.UserID, fixture.objectID)
				if readError != nil || object.Status != domain.StoredObjectStatusStaging || object.StorageETag != nil || object.StorageVersionID != nil {
					t.Fatal("结果未确认时不应留下半条写回", readError)
				}
				execute(t, "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE id=?", fixture.taskID)
				firstVersion := lease.LeaseVersion
				lease = claim("fixture-recovery")
				if lease.LeaseVersion != firstVersion+1 {
					t.Fatal("恢复夹具未领取新版本")
				}
				result, err = execution.Execute(ctx, lease)
			}

			// 3. 核对状态、确认版本和原配额，确认每条任务只执行过一次存储合并。
			if err != nil || result == nil || result.Status != domain.UploadStatusVerifying || readTask().Status != domain.UploadStatusVerifying || completeCalls != 1 {
				t.Fatal("单条执行未得到已提交的校验任务", result, err, completeCalls)
			}
			object, err := store.FindUserStoredObject(ctx, actor.UserID, fixture.objectID)
			if err != nil || object.Status != domain.StoredObjectStatusVerifying || object.StorageETag == nil || object.StorageVersionID == nil || object.VerifiedAt != nil {
				t.Fatal("未保存确认版本，或过早标记已校验", err)
			}
			metadata, err := client.StatMultipartUploadObject(ctx, fixture.key)
			if err != nil || metadata.SizeBytes != fileSize || metadata.StorageETag == nil || *metadata.StorageETag != *object.StorageETag ||
				metadata.StorageVersionID == nil || *metadata.StorageVersionID != *object.StorageVersionID {
				t.Fatal("写回元数据与存储版本不一致", err)
			}
			var reserved, used uint64
			if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,used_storage FROM users WHERE id=7").Scan(&reserved, &used); err != nil || reserved != 2*fileSize || used != 0 {
				t.Fatal("单条合并改变了账户配额", reserved, used, err)
			}
			t.Logf("任务 %d 进入 %s，确认版本 %s，合并调用 %d 次", fixture.taskID, result.Status, *object.StorageVersionID, completeCalls)
		})
	}
}
