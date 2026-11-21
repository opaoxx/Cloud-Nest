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

	"github.com/go-sql-driver/mysql"
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
	const reservedStorage = 5 * fileSize
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 41}
	execute := func(t *testing.T, statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	execute(t, "INSERT INTO users(id,username,display_name,password_hash,reserved_storage) VALUES(7,'multipart_merge_fixture','分片合并','fixture_hash',?)", reservedStorage)
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
		name, key, fault string
		taskID, objectID uint64
	}{
		{"new completion", "b4/execution-normal.bin", "", 19, 23},
		{"recover confirmed version after lost response", "b4/execution-recovered.bin", "response", 20, 24},
		{"recover confirmed version after write fault", "b4/execution-write-fault.bin", "write", 21, 25},
		{"repair a part replaced before preflight", "b4/execution-preflight-repair.bin", "early replacement", 22, 26},
		{"repair a part rejected by storage", "b4/execution-complete-repair.bin", "late replacement", 23, 27},
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

			// 2. 在合成任务上制造指定故障，并执行单条合并。
			earlyReplacement, lateReplacement := fixture.fault == "early replacement", fixture.fault == "late replacement"
			replaceTail := func() {
				t.Helper()
				replacement := []byte{0x44}
				checksum := multipartProbeSHA256(replacement)
				putMultipartProbePart(t, ctx, transport, probe.sign(t, ctx, 2, len(replacement), checksum), checksum, replacement, http.StatusOK, "")
			}
			if earlyReplacement {
				replaceTail()
			}
			if fixture.fault == "write" {
				execute(t, `CREATE TRIGGER fixture_merge_lock BEFORE UPDATE ON stored_objects FOR EACH ROW
					SIGNAL SQLSTATE 'HY000' SET MYSQL_ERRNO=1205, MESSAGE_TEXT='fixture lock wait'`)
			}
			completeCalls := 0
			execution := service.NewUploadMergeExecutionService(settings.Provider, settings.Bucket, preflight.Check,
				func(callContext context.Context, key, uploadID string, size, partSize uint64, count uint32, receipts []domain.UploadPartReceipt) (*domain.UploadObjectResult, error) {
					completeCalls++
					if lateReplacement {
						replaceTail()
					}
					confirmation, err := client.CompleteMultipartUpload(callContext, key, uploadID, size, partSize, count, receipts)
					if lateReplacement && !errors.Is(err, domain.ErrMultipartUploadPartRejected) {
						t.Fatal("存储未明确拒绝已替换的分片回执", err)
					}
					if err == nil && fixture.fault == "response" {
						return nil, domain.ErrMultipartUploadCompletionUnknown
					}
					return confirmation, err
				}, store.BeginMultipartUploadVerification, store.ScheduleMultipartUploadMergeRetry, preflight.CheckRepair, store.ReopenMultipartUploadForRepair)
			result, err := execution.Execute(ctx, lease)
			assertQuotaUnchanged := func() {
				t.Helper()
				var reserved, used uint64
				if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,used_storage FROM users WHERE id=7").Scan(&reserved, &used); err != nil || reserved != reservedStorage || used != 0 {
					t.Fatal("单条合并改变了账户配额", reserved, used, err)
				}
			}

			// 3. 核对失配修复的持久状态、原会话及旧租约无法重放。
			if earlyReplacement || lateReplacement {
				wantCompleteCalls := 0
				if lateReplacement {
					wantCompleteCalls = 1
				}
				if err != nil || result == nil || result.Status != domain.UploadStatusUploading || completeCalls != wantCompleteCalls {
					t.Fatal("失配后未恢复补传，或额外调用了合并", result, err, completeCalls)
				}
				persisted := readTask()
				if persisted.Status != domain.UploadStatusUploading || persisted.StorageUploadID == nil || *persisted.StorageUploadID != uploadID ||
					persisted.StagedObjectID != fixture.objectID || persisted.LeaseVersion != lease.LeaseVersion || persisted.LeaseOwner != nil ||
					persisted.LeaseUntil != nil || persisted.NextAttemptAt != nil || persisted.ReservedBytes != fileSize {
					t.Fatal("补传任务未保留原会话、原预占和绑定", persisted)
				}
				if replayed, replayError := execution.Execute(ctx, lease); replayed != nil || !errors.Is(replayError, domain.ErrUploadTaskNotReady) ||
					completeCalls != wantCompleteCalls {
					t.Fatal("原租约仍可执行", replayed, replayError, completeCalls)
				}
				var receiptCount int
				if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM upload_parts WHERE task_id=?", fixture.taskID).Scan(&receiptCount); err != nil || receiptCount != 0 {
					t.Fatal("旧冻结回执未清空", receiptCount, err)
				}
				updatedParts, err := client.ListUploadParts(ctx, fixture.key, uploadID, 1)
				if err != nil || len(updatedParts) != 2 || updatedParts[0] != parts[0] || updatedParts[1].ETag == parts[1].ETag {
					t.Fatal("原会话未保留已上传分片及最新回执", err)
				}
				if _, err := client.StatMultipartUploadObject(ctx, fixture.key); !errors.Is(err, domain.ErrUploadObjectMissing) {
					t.Fatal("补传过程中不应生成对象", err)
				}
				assertQuotaUnchanged()
				t.Logf("任务 %d 恢复 %s，旧回执 %d 条，原会话可读取，合并调用 %d 次", fixture.taskID, persisted.Status, receiptCount, completeCalls)
				return
			}

			// 4. 对未知结果登记重试的场景，重新领取租约并从原位置确认版本。
			if fixture.fault != "" {
				var retry *service.UploadMergeRetryScheduledError
				scheduled := readTask()
				if result != nil || !errors.As(err, &retry) || scheduled.Status != domain.UploadStatusCompleting || completeCalls != 1 ||
					scheduled.NextAttemptAt == nil || !scheduled.NextAttemptAt.Equal(retry.NextAttemptAt) || scheduled.NextAttemptAt.Before(*lease.LeaseUntil) ||
					scheduled.LeaseOwner != nil || scheduled.LeaseUntil != nil || scheduled.AttemptCount != lease.AttemptCount+1 {
					t.Fatal("首轮未按旧租约安全窗口登记重试", result, err, completeCalls)
				}
				if fixture.fault == "response" && !errors.Is(err, domain.ErrMultipartUploadCompletionUnknown) {
					t.Fatal("响应丢失的原错误未保留", err)
				}
				if fixture.fault == "write" {
					var mysqlError *mysql.MySQLError
					if !errors.Is(err, domain.ErrUploadMergeWriteRetryable) || !errors.As(err, &mysqlError) || mysqlError.Number != 1205 {
						t.Fatal("写回事务的 MySQL 原错误未保留", err)
					}
					execute(t, "DROP TRIGGER fixture_merge_lock")
				}
				if candidates, scanError := store.ListMultipartUploadCompletionCandidates(ctx, 1); scanError != nil || len(candidates) != 0 {
					t.Fatal("原租约安全窗口内提前选入候选", candidates, scanError)
				}
				object, readError := store.FindUserStoredObject(ctx, actor.UserID, fixture.objectID)
				if readError != nil || object.Status != domain.StoredObjectStatusStaging || object.StorageETag != nil || object.StorageVersionID != nil {
					t.Fatal("结果未确认时不应留下半条写回", readError)
				}
				execute(t, "UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6) WHERE id=?", fixture.taskID)
				firstVersion := lease.LeaseVersion
				lease = claim("fixture-recovery")
				if lease.LeaseVersion != firstVersion+1 {
					t.Fatal("恢复夹具未领取新版本")
				}
				result, err = execution.Execute(ctx, lease)
			}

			// 5. 核对状态、确认版本和原配额，确认每条任务只执行过一次存储合并。
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
			assertQuotaUnchanged()
			t.Logf("任务 %d 进入 %s，确认版本 %s，合并调用 %d 次", fixture.taskID, result.Status, *object.StorageVersionID, completeCalls)
		})
	}
}
