package main

import (
	"context"
	"errors"
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

func TestServerMultipartUploadInitializationIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_INITIALIZATION_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_INITIALIZATION_TEST=1 后使用独占 MinIO 验证初始化")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	container := startIsolatedWebMinIO(t, ctx)
	bucket := prepareServerUploadContentBucket(t, ctx)
	settings, err := config.LoadStorageConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := storage.NewClient(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	const key = "users/7/objects/multipart-initialization-fixture"

	// 1. 使用真实适配器创建，再按原对象键和返回 ID 核对未完成会话。
	id, err := client.CreateMultipartUpload(ctx, key)
	if err != nil || id == "" {
		t.Fatal("独占桶初始化失败", err)
	}
	core := minio.Core{Client: bucket.client}
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := core.AbortMultipartUpload(cleanupContext, bucket.name, key, id); err != nil {
			t.Error("清理本轮测试会话失败", minio.ToErrorResponse(err).Code)
		}
	})
	page, err := core.ListObjectParts(ctx, bucket.name, key, id, 0, 1)
	if err != nil || page.UploadID != id || page.Bucket != bucket.name || page.Key != key ||
		page.ChecksumAlgorithm != "SHA256" || page.IsTruncated || len(page.ObjectParts) != 0 {
		t.Fatal("初始化后的空会话、位置或校验算法不符", minio.ToErrorResponse(err).Code)
	}

	// 2. 验证预取消无新增会话，桶内尚无已提交对象版本。
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if id, err := client.CreateMultipartUpload(cancelled, key); id != "" || !errors.Is(err, context.Canceled) {
		t.Fatal("预取消初始化未按预期拒绝", err)
	}
	incomplete, err := core.ListMultipartUploads(ctx, bucket.name, key, "", "", "", 10)
	if err != nil || incomplete.IsTruncated || len(incomplete.Uploads) != 1 ||
		incomplete.Uploads[0].Key != key || incomplete.Uploads[0].UploadID != id {
		t.Fatal("本轮隔离桶未完成会话与预期不符", minio.ToErrorResponse(err).Code)
	}
	for item := range bucket.client.ListObjects(ctx, bucket.name, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
		t.Fatal("初始化后隔离桶不应包含已提交对象", minio.ToErrorResponse(item.Err).Code)
	}
	t.Logf("隔离 MinIO %s，端点 %s，桶 %s，确认一条 SHA256 未完成会话与零分片", container, settings.EndpointURL, bucket.name)
}

func TestServerMultipartUploadInitializationProcessIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_INITIALIZATION_PROCESS_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_INITIALIZATION_PROCESS_TEST=1 后使用独占 MySQL 和 MinIO")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	mysqlContainer := startIsolatedWebMySQL(t, ctx)
	minioContainer := startIsolatedWebMinIO(t, ctx)
	t.Setenv("CLOUDNEST_STORAGE_PROVIDER", "local_minio")
	bucket := prepareServerUploadContentBucket(t, ctx)
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
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
	const key = "users/7/objects/multipart-process-fixture"
	const size = 16777217
	const worker = "multipart-test-worker"
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}

	// 1. 在专用库登记一份任务及预占，随后每次都通过仓储扫描取候选。
	execute("INSERT INTO users(id,username,display_name,password_hash,reserved_storage) VALUES(7,'multipart_fixture','分片夹具','fixture_hash',?)", size)
	execute("INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(23,7,?,?,?,?)", settings.Provider, bucket.name, key, size)
	execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
		expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at,attempt_count)
		VALUES(19,7,11,23,13,'fixture.bin','MULTIPART',UNHEX(REPEAT('ab',32)),?,16777216,2,'CREATING',?,UTC_TIMESTAMP(6)+INTERVAL 1 DAY,5)`, size, size)
	read := func(callContext context.Context) *domain.UploadTask {
		t.Helper()
		task, err := store.FindUserUploadTask(callContext, 7, 19)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	scan := func(expectedCount int) []domain.MultipartUploadInitializationCandidate {
		t.Helper()
		candidates, err := store.ListMultipartUploadInitializationCandidates(ctx, 10)
		if err != nil || len(candidates) != expectedCount {
			t.Fatal("隔离候选数量与预期不符", len(candidates), err)
		}
		return candidates
	}
	core := minio.Core{Client: bucket.client}
	var createdIDs []string
	create := func(callContext context.Context, objectKey string) (string, error) {
		// 存储调用前，从独立连接读到已提交的租约和未绑定的任务。
		task := read(callContext)
		if objectKey != key || task.Status != domain.UploadStatusCreating || task.StorageUploadID != nil ||
			task.LeaseOwner == nil || *task.LeaseOwner != worker || task.LeaseVersion != uint64(len(createdIDs)+1) {
			t.Fatal("存储创建未使用已提交的租约和原对象键")
		}
		id, err := client.CreateMultipartUpload(callContext, objectKey)
		if err == nil {
			createdIDs = append(createdIDs, id)
			t.Cleanup(func() {
				cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
				defer stop()
				if err := core.AbortMultipartUpload(cleanup, bucket.name, key, id); err != nil {
					t.Error("清理本轮隔离会话失败", minio.ToErrorResponse(err).Code)
				}
			})
		}
		return id, err
	}
	newProcessor := func(createSession func(context.Context, string) (string, error)) *service.MultipartUploadInitializationProcessor {
		return service.NewMultipartUploadInitializationProcessor(settings.Provider, settings.Bucket,
			store.ClaimMultipartUploadInitializationLease, store.FindUserStoredObject, createSession,
			store.BindMultipartUploadSession, store.ScheduleMultipartUploadInitializationRetry)
	}
	checkQuota := func(task *domain.UploadTask) {
		t.Helper()
		var reserved, used uint64
		err := pool.QueryRowContext(ctx, "SELECT reserved_storage,used_storage FROM users WHERE id=7").Scan(&reserved, &used)
		if err != nil || reserved != size || used != 0 || task.ReservedBytes != size || task.FileSize != size || task.PartCount == nil || *task.PartCount != 2 {
			t.Fatal("任务分片计划或账号预占发生变化", err)
		}
	}
	checkRecoverable := func(version uint64) {
		t.Helper()
		task := read(ctx)
		checkQuota(task)
		if task.Status != domain.UploadStatusCreating || task.StorageUploadID != nil || task.LeaseVersion != version || task.AttemptCount != 6 ||
			task.LeaseOwner == nil || *task.LeaseOwner != worker {
			t.Fatal("失败后未保留可恢复的初始化任务")
		}
		scan(0)
		execute("UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6) WHERE user_id=7 AND id=19")
	}

	// 2. 存储端已创建但响应超时，登记重试并检查等待期间跳过扫描。
	timeoutProcessor := newProcessor(func(callContext context.Context, objectKey string) (string, error) {
		if _, err := create(callContext, objectKey); err != nil {
			return "", err
		}
		return "", context.DeadlineExceeded
	})
	scheduled, err := timeoutProcessor.Process(ctx, scan(1)[0], worker)
	if err != nil || scheduled == nil || scheduled.Status != domain.UploadStatusCreating || scheduled.StorageUploadID != nil ||
		scheduled.LeaseVersion != 1 || scheduled.LeaseOwner != nil || scheduled.LeaseUntil != nil || scheduled.AttemptCount != 6 ||
		scheduled.NextAttemptAt == nil || !scheduled.NextAttemptAt.Equal(scheduled.UpdatedAt.Add(30*time.Second)) ||
		scheduled.LastErrorCode == nil || *scheduled.LastErrorCode != "STORAGE_UNAVAILABLE" {
		t.Fatal("存储超时未提交当前任务的重试安排", err)
	}
	current := read(ctx)
	if current.NextAttemptAt == nil || !current.NextAttemptAt.Equal(*scheduled.NextAttemptAt) ||
		current.LeaseOwner != nil || current.LeaseUntil != nil || current.AttemptCount != 6 || current.StorageUploadID != nil {
		t.Fatal("独立读回与已提交的重试安排不符")
	}
	checkQuota(current)
	scan(0)
	execute("UPDATE upload_tasks SET next_attempt_at=UTC_TIMESTAMP(6) WHERE user_id=7 AND id=19")

	// 3. 模拟绑定事务回滚，存储重试次数保持本轮已登记的值。
	processor := newProcessor(create)
	execute("CREATE TRIGGER fixture_fail_bind BEFORE UPDATE ON upload_tasks FOR EACH ROW SET NEW.part_count=IF(NEW.status='UPLOADING',3,NEW.part_count)")
	if result, err := processor.Process(ctx, scan(1)[0], worker); result != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
		t.Fatal("写回回滚被当作成功", err)
	}
	execute("DROP TRIGGER fixture_fail_bind")
	checkRecoverable(2)

	// 4. 原任务重新领租并完成绑定，核对已提交任务与 MinIO 会话。
	candidate := scan(1)[0]
	task, err := processor.Process(ctx, candidate, worker)
	if err != nil || task == nil || task.ID != 19 || task.Status != domain.UploadStatusUploading || task.StorageUploadID == nil ||
		len(createdIDs) != 3 || *task.StorageUploadID != createdIDs[2] || task.LeaseVersion != 3 || task.LeaseOwner != nil ||
		task.NextAttemptAt != nil || task.LastErrorCode != nil || task.AttemptCount != 6 {
		t.Fatal("重新初始化未返回本轮绑定的已提交任务", err)
	}
	checkQuota(task)
	if current := read(ctx); current.StorageUploadID == nil || *current.StorageUploadID != *task.StorageUploadID ||
		current.Status != task.Status || current.LeaseVersion != task.LeaseVersion {
		t.Fatal("从独立连接读取的绑定结果与返回值不同")
	}
	scan(0)
	if result, err := processor.Process(ctx, candidate, worker); result != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) {
		t.Fatal("已绑定任务被旧候选重复处理", err)
	}
	incomplete, err := core.ListMultipartUploads(ctx, bucket.name, key, "", "", "", 10)
	if err != nil || incomplete.IsTruncated || len(incomplete.Uploads) != 3 {
		t.Fatal("隔离桶会话数量不符", len(incomplete.Uploads), minio.ToErrorResponse(err).Code)
	}
	page, err := core.ListObjectParts(ctx, bucket.name, key, *task.StorageUploadID, 0, 1)
	if err != nil || page.Key != key || page.UploadID != *task.StorageUploadID || len(page.ObjectParts) != 0 || page.ChecksumAlgorithm != "SHA256" {
		t.Fatal("绑定的会话位置或 SHA256 算法不符", minio.ToErrorResponse(err).Code)
	}
	t.Logf("隔离 MySQL %s %s，MinIO %s %s，确认 30 秒重试，同一任务最终绑定 1 个会话并保留 2 个待核对会话",
		mysqlContainer[:12], databaseName, minioContainer[:12], bucket.name)
}
