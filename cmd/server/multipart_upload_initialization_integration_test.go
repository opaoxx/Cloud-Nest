package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
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
	execute("INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(41,7,11,UNHEX(REPEAT('41',32)),UTC_TIMESTAMP(6)+INTERVAL 1 DAY)")
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(23,7,?,?,?,?)", settings.Provider, bucket.name, key, size)
	execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,
		expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at,attempt_count)
		VALUES(19,7,11,23,13,'fixture.bin','MULTIPART',UNHEX(REPEAT('ab',32)),?,16777216,2,'CREATING',?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR,5)`, size, size)
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
	rememberSession := func(id string) {
		createdIDs = append(createdIDs, id)
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			if err := core.AbortMultipartUpload(cleanup, bucket.name, key, id); err != nil {
				t.Error("清理本轮隔离会话失败", minio.ToErrorResponse(err).Code)
			}
		})
	}
	create := func(callContext context.Context, objectKey string) (string, error) {
		// 存储调用前，从独立连接读到已提交的租约和未绑定的任务。
		task := read(callContext)
		if objectKey != key || task.Status != domain.UploadStatusCreating || task.StorageUploadID != nil ||
			task.LeaseOwner == nil || *task.LeaseOwner != worker || task.LeaseVersion != uint64(len(createdIDs)+1) {
			t.Fatal("存储创建未使用已提交的租约和原对象键")
		}
		id, err := client.CreateMultipartUpload(callContext, objectKey)
		if err == nil {
			rememberSession(id)
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
	timeoutBatch := service.NewMultipartUploadInitializationBatch(store.ListMultipartUploadInitializationCandidates, timeoutProcessor.Process)
	summary, err := timeoutBatch.RunOnce(ctx, 1, worker)
	if err != nil || summary.Scanned != 1 || summary.Bound != 0 || summary.RetriesScheduled != 1 || len(summary.CandidateErrors) != 0 {
		t.Fatal("本轮未汇总已提交的重试安排", summary, err)
	}
	scheduled := read(ctx)
	if scheduled.Status != domain.UploadStatusCreating || scheduled.StorageUploadID != nil ||
		scheduled.LeaseVersion != 1 || scheduled.LeaseOwner != nil || scheduled.LeaseUntil != nil || scheduled.AttemptCount != 6 ||
		scheduled.NextAttemptAt == nil || !scheduled.NextAttemptAt.Equal(scheduled.UpdatedAt.Add(30*time.Second)) ||
		scheduled.LastErrorCode == nil || *scheduled.LastErrorCode != "STORAGE_UNAVAILABLE" {
		t.Fatal("存储超时未提交当前任务的重试安排")
	}
	checkQuota(scheduled)
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

	// 4. 使用正式服务装配完成绑定，核对已提交任务与 MinIO 会话。
	candidate := scan(1)[0]
	batch := newServerMultipartUploadInitializationBatch(store, client, settings)
	summary, err = batch.RunOnce(ctx, multipartUploadInitializationBatchLimit, worker)
	if err != nil || summary.Scanned != 1 || summary.Bound != 1 || summary.RetriesScheduled != 0 || len(summary.CandidateErrors) != 0 {
		t.Fatal("本轮未汇总已提交的会话绑定", summary, err)
	}
	task := read(ctx)
	if task.StorageUploadID == nil || len(createdIDs) != 2 || *task.StorageUploadID == createdIDs[0] || *task.StorageUploadID == createdIDs[1] {
		t.Fatal("服务装配未绑定本轮创建的新会话")
	}
	rememberSession(*task.StorageUploadID)
	if task.ID != 19 || task.Status != domain.UploadStatusUploading || task.LeaseVersion != 3 || task.LeaseOwner != nil ||
		task.NextAttemptAt != nil || task.LastErrorCode != nil || task.AttemptCount != 6 {
		t.Fatal("重新初始化未提交本轮绑定的任务")
	}
	checkQuota(task)
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

	// 5. 普通签发保留原期限，显式续期后使用实际尾片链接。
	targets := service.NewUploadPartSigningTargetService(settings.Provider, settings.Bucket, store.FindUserUploadTaskForPartSigning, store.FindUserStoredObject)
	urls := service.NewUploadPartURLService(targets.Get, client.SignUploadPartURL, store.RenewUserUploadPartExpiry)
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 41}
	tail := []byte{0x33}
	declarations := []service.UploadPartDeclaration{
		{PartNumber: 2, SHA256: fmt.Sprintf("%x", sha256.Sum256(tail))},
		{PartNumber: 1, SHA256: fmt.Sprintf("%x", *task.ExpectedSHA256)},
	}
	oldExpiry := task.ExpiresAt
	ordinary, err := urls.Create(ctx, actor, task.ID, declarations, nil)
	afterOrdinary := read(ctx)
	if err != nil || ordinary == nil || !ordinary.ExpiresAt.Equal(oldExpiry) ||
		!afterOrdinary.ExpiresAt.Equal(oldExpiry) || !afterOrdinary.UpdatedAt.Equal(task.UpdatedAt) {
		t.Fatal("普通签发没有保留原任务期限与更新时间", err)
	}
	var beforeRenewal time.Time
	if err := pool.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&beforeRenewal); err != nil {
		t.Fatal(err)
	}
	signedBatch, err := urls.Create(ctx, actor, task.ID, declarations, &oldExpiry)
	renewed := read(ctx)
	checkQuota(renewed)
	if err != nil || signedBatch == nil || len(signedBatch.Parts) != 2 || !signedBatch.ExpiresAt.Equal(renewed.ExpiresAt) ||
		renewed.ExpiresAt.Before(beforeRenewal.Add(24*time.Hour)) || renewed.ExpiresAt.After(renewed.UpdatedAt.Add(24*time.Hour)) ||
		signedBatch.Parts[0].PartNumber != 2 || signedBatch.Parts[0].SizeBytes != 1 ||
		signedBatch.Parts[1].PartNumber != 1 || signedBatch.Parts[1].SizeBytes != 16777216 {
		t.Fatal("未按提交后的期限交付已复查的两片签名", err)
	}
	if result, err := urls.Create(ctx, actor, task.ID, declarations, &oldExpiry); result != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) {
		t.Fatal("旧期限被重复续期或签发", err)
	}
	tailURL := signedBatch.Parts[0]
	signed, err := url.Parse(tailURL.URL)
	if err != nil || signed.Host != settings.PublicEndpointURL.Host || tailURL.ChecksumSHA256 != multipartProbeSHA256(tail) {
		t.Fatal("尾片签名主机或摘要不符")
	}
	httpClient := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(httpClient.CloseIdleConnections)
	etag := putMultipartProbePart(t, ctx, httpClient, signed, tailURL.ChecksumSHA256, tail, http.StatusOK, "")
	probe := multipartProbe{core: core, bucket: bucket.name, key: key, uploadID: *task.StorageUploadID}
	if page := probe.readPartPage(t, ctx, 0, 2, len(tail), etag, tailURL.ChecksumSHA256); page.IsTruncated {
		t.Fatal("尾片上传后出现其他分片")
	}

	// 6. 写入一个长度不符的合成分片，验证只读服务完整跨页并仅保留尾片。
	shortFirst := []byte{0x42}
	shortChecksum := multipartProbeSHA256(shortFirst)
	shortLink, err := client.SignUploadPartURL(ctx, key, *task.StorageUploadID, 1, uint64(len(shortFirst)), shortChecksum, renewed.ExpiresAt)
	if err != nil || shortLink == nil {
		t.Fatal("合成短分片签发失败", err)
	}
	shortURL, err := url.Parse(shortLink.URL)
	if err != nil || shortURL.Host != settings.PublicEndpointURL.Host || shortURL.Scheme != settings.PublicEndpointURL.Scheme ||
		shortURL.Path != "/"+bucket.name+"/"+key || shortURL.Query().Get("uploadId") != *task.StorageUploadID {
		t.Fatal("短分片签名未限定在本轮独占会话")
	}
	shortETag := putMultipartProbePart(t, ctx, httpClient, shortURL, shortChecksum, shortFirst, http.StatusOK, "")
	receiptService := service.NewUploadPartReceiptService(targets.GetForReceipts,
		func(callContext context.Context, objectKey, session string) ([]domain.UploadPartReceipt, error) {
			return client.ListUploadParts(callContext, objectKey, session, 1)
		})
	beforeReceipts := read(ctx)
	received, err := receiptService.List(ctx, actor, task.ID)
	wantReceipt := domain.UploadPartReceipt{PartNumber: 2, SizeBytes: 1, ETag: etag, ChecksumSHA256: tailURL.ChecksumSHA256}
	if err != nil || received == nil || received.PartSizeBytes != 16777216 || received.PartCount != 2 ||
		len(received.Parts) != 1 || received.Parts[0] != wantReceipt {
		t.Fatal("只读服务未按已保存的计划筛选真实两页回执", err)
	}
	afterReceipts := read(ctx)
	checkQuota(afterReceipts)
	if !reflect.DeepEqual(beforeReceipts, afterReceipts) {
		t.Fatal("只读核对改变了上传任务")
	}

	// 7. 在专用库模拟签发途中租约版本变化，整批结果必须作废。
	staleURLs := service.NewUploadPartURLService(targets.Get,
		func(callContext context.Context, objectKey, session string, partNumber uint32, sizeBytes uint64, checksum string, expiresAt time.Time) (*domain.SignedUploadPartURL, error) {
			link, err := client.SignUploadPartURL(callContext, objectKey, session, partNumber, sizeBytes, checksum, expiresAt)
			if err == nil && partNumber == 1 {
				execute("UPDATE upload_tasks SET lease_version=lease_version+1 WHERE id=19 AND user_id=7")
			}
			return link, err
		}, store.RenewUserUploadPartExpiry)
	if result, err := staleURLs.Create(ctx, actor, task.ID, declarations, nil); result != nil || !errors.Is(err, domain.ErrUploadTaskNotReady) {
		t.Fatal("签发途中租约版本变化仍交付了链接", err)
	}

	// 8. 在提交续期后模拟第二片签名失败，检查首片未交付且已提交期限仍在。
	execute("UPDATE upload_tasks SET expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE id=19 AND user_id=7")
	oldExpiry = read(ctx).ExpiresAt
	failingURLs := service.NewUploadPartURLService(targets.Get,
		func(callContext context.Context, objectKey, session string, partNumber uint32, sizeBytes uint64, checksum string, expiresAt time.Time) (*domain.SignedUploadPartURL, error) {
			committed := read(callContext)
			if !committed.ExpiresAt.Equal(expiresAt) || !expiresAt.After(oldExpiry) {
				t.Fatal("存储签名之前未提交续期")
			}
			if partNumber == 1 {
				return nil, domain.ErrUploadStorageUnavailable
			}
			return client.SignUploadPartURL(callContext, objectKey, session, partNumber, sizeBytes, checksum, expiresAt)
		}, store.RenewUserUploadPartExpiry)
	if result, err := failingURLs.Create(ctx, actor, task.ID, declarations, &oldExpiry); result != nil || !errors.Is(err, domain.ErrUploadStorageUnavailable) {
		t.Fatal("第二片签名失败仍交付了部分链接", err)
	}
	afterFailure := read(ctx)
	if !afterFailure.ExpiresAt.After(oldExpiry) {
		t.Fatal("第二片签名失败后已提交的期限丢失")
	}
	checkQuota(afterFailure)

	// 9. 补齐真实的普通片，在完成核对租约下读完两页并拒绝覆盖前的 ETag。
	fullFirst := make([]byte, size-1)
	firstChecksum := multipartProbeSHA256(fullFirst)
	firstLink, err := client.SignUploadPartURL(ctx, key, *afterFailure.StorageUploadID, 1, uint64(len(fullFirst)), firstChecksum, afterFailure.ExpiresAt)
	if err != nil || firstLink == nil {
		t.Fatal("签发合成完整分片失败", err)
	}
	firstURL, err := url.Parse(firstLink.URL)
	if err != nil || firstURL.Host != settings.PublicEndpointURL.Host || firstURL.Scheme != settings.PublicEndpointURL.Scheme ||
		firstURL.User != nil || firstURL.Path != "/"+bucket.name+"/"+key || firstURL.Query().Get("uploadId") != *afterFailure.StorageUploadID ||
		firstURL.Query().Get("partNumber") != "1" {
		t.Fatal("完整分片地址未限制在本轮独占会话")
	}
	firstETag := putMultipartProbePart(t, ctx, httpClient, firstURL, firstChecksum, fullFirst, http.StatusOK, "")
	leased, err := store.ClaimUserMultipartCompletionLease(ctx, actor, afterFailure, "fixture-completion-verifier", service.ResolveMultipartUploadCompletionStatus)
	if err != nil || leased == nil {
		t.Fatal("独占任务领取完成核对租约失败", err)
	}
	verifier := service.NewUploadCompletionReceiptService(targets.GetForCompletion,
		func(callContext context.Context, objectKey, session string) ([]domain.UploadPartReceipt, error) {
			return client.ListUploadParts(callContext, objectKey, session, 1)
		})
	claims := []domain.UploadPartCompletionClaim{{PartNumber: 2, ETag: etag}, {PartNumber: 1, ETag: shortETag}}
	var mismatch *domain.UploadPartsMismatchError
	if result, err := verifier.Verify(ctx, actor, leased, claims); result != nil || !errors.As(err, &mismatch) ||
		!slices.Equal(mismatch.PartNumbers, []uint32{1}) {
		t.Fatal("未拒绝覆盖前的普通片 ETag", err)
	}
	claims[1].ETag = firstETag
	want := []domain.UploadPartReceipt{
		{PartNumber: 1, SizeBytes: size - 1, ETag: firstETag, ChecksumSHA256: firstChecksum},
		wantReceipt,
	}
	matched, err := verifier.Verify(ctx, actor, leased, claims)
	if err != nil || !slices.Equal(matched, want) || !reflect.DeepEqual(leased, read(ctx)) {
		t.Fatal("持租约核对未交付两页真实回执，或修改了任务", err)
	}
	checkQuota(leased)
	if released, err := store.ReleaseUserMultipartCompletionLease(ctx, actor, leased); err != nil || !released {
		t.Fatal("释放本轮独占任务的测试租约失败", err)
	}
	t.Logf("持租约核对实际两页，普通片 %d 字节，尾片 %d 字节，旧 ETag 失配编号 [1]",
		matched[0].SizeBytes, matched[1].SizeBytes)

	t.Logf("隔离 MySQL %s %s，MinIO %s %s，签发、续期、尾片 PUT 与只读两页回执核对通过",
		mysqlContainer[:12], databaseName, minioContainer[:12], bucket.name)
}
