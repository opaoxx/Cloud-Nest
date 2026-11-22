package main

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartSessionCandidatePageIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_SESSION_CANDIDATE_PAGE_TEST") != "1" {
		t.Skip("显式启用后创建专用 MySQL、MinIO 和合成会话")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	mysqlID := startIsolatedWebMySQL(t, ctx)
	t.Logf("专用 MySQL %s，回环地址 %s", mysqlID[:12], os.Getenv("CLOUDNEST_DB_ADDRESS"))
	minioID := startIsolatedWebMinIO(t, ctx)
	t.Logf("专用 MinIO %s，回环地址 %s", minioID[:12], os.Getenv("CLOUDNEST_S3_ENDPOINT"))
	prepareServerUploadContentBucket(t, ctx)
	pool, _ := prepareServerIntegrationDatabase(t, ctx)
	pool.SetMaxOpenConns(1)
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
	key, err := service.GenerateUploadObjectKey(7)
	if err != nil {
		t.Fatal(err)
	}

	// 1. 同一键下创建三条会话，只有第一条写为任务当前绑定。
	createSession := func(objectKey string) string {
		t.Helper()
		uploadID, err := client.CreateMultipartUpload(ctx, objectKey)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := client.AbortMultipartUpload(cleanup, objectKey, uploadID); err != nil && !errors.Is(err, domain.ErrUploadPartSessionMissing) {
				t.Error("合成会话清理失败", err)
			}
		})
		return uploadID
	}
	var sessions []string
	for range 3 {
		sessions = append(sessions, createSession(key))
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	const partSize, fileSize = 16 << 20, (16 << 20) + 1
	clock := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	execute("SET timestamp = ?", clock.Unix())
	defer execute("SET timestamp = DEFAULT")
	execute("INSERT INTO users(id,username,display_name,password_hash,used_storage,reserved_storage) VALUES(7,'candidate_fixture','合成账号','fixture_hash',1024,?)", fileSize)
	execute("INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")
	execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(23,7,?,?,?,?)", settings.Provider, settings.Bucket, key, fileSize)
	execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
		expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
		VALUES(19,7,11,23,13,'fixture.bin','MULTIPART',?,UNHEX(REPEAT('ab',32)),?,?,2,'UPLOADING',?,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`,
		sessions[0], fileSize, partSize, fileSize)
	targets, err := store.ListMultipartUploadSessionScanTargets(ctx, settings.Provider, settings.Bucket, 0, 1)
	if err != nil || targets == nil || len(targets.Targets) != 1 {
		t.Fatal("未取得原任务的对象键", err)
	}
	target := targets.Targets[0]
	scanner := service.NewMultipartUploadSessionCandidateScanner(settings.Provider, settings.Bucket, client.ListMultipartUploadSessionsPage,
		store.ReadMultipartUploadSessionCleanupFacts)

	// 2. 用一页预算暂停，再从保存的游标继续读到末页。
	const grace = 15 * time.Minute
	first, err := scanner.ScanPages(ctx, target, "", 2, 1, grace)
	if err != nil || first.PagesScanned != 1 || first.Scanned != 2 || first.SkippedBound != 1 || len(first.Failures) != 0 || len(first.Candidates) != 1 ||
		first.Candidates[0].UploadID != sessions[1] || first.NextUploadID != sessions[1] {
		t.Fatal("第一页绑定或超期结果不符", err)
	}
	second, err := scanner.ScanPages(ctx, target, first.NextUploadID, 2, 2, grace)
	if err != nil || second.PagesScanned != 1 || second.Scanned != 1 || len(second.Failures) != 0 || len(second.Candidates) != 1 ||
		second.Candidates[0].UploadID != sessions[2] || second.NextUploadID != "" {
		t.Fatal("第二页未按原游标续读", err)
	}
	// 3. 在同一轮跨页汇总不同保护，再确认任务与会话仍可读取。
	fresh, err := scanner.ScanPages(ctx, target, "", 2, 2, 2*time.Hour)
	if err != nil || fresh.PagesScanned != 2 || fresh.Scanned != 3 || fresh.NextUploadID != "" || fresh.SkippedBound != 1 ||
		fresh.SkippedWithinGrace != 2 || len(fresh.Candidates) != 0 || len(fresh.Failures) != 0 {
		t.Fatal("仍在宽限期的会话未正确跳过", err)
	}
	execute("UPDATE upload_tasks SET status='CREATING',storage_upload_id=NULL,lease_owner='fixture-creator',lease_version=1,lease_until=UTC_TIMESTAMP(6)+INTERVAL 1 MINUTE WHERE id=19")
	before, err := store.FindUserUploadTask(ctx, 7, 19)
	if err != nil {
		t.Fatal(err)
	}
	protected, err := scanner.ScanPages(ctx, target, "", 2, 2, grace)
	if err != nil || protected.PagesScanned != 2 || protected.Scanned != 3 || protected.NextUploadID != "" ||
		protected.SkippedCreation != 3 || len(protected.Candidates) != 0 || len(protected.Failures) != 0 {
		t.Fatal("有效创建租约未保护原键下的会话", err)
	}
	after, err := store.FindUserUploadTask(ctx, 7, 19)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("只读扫描改变了任务记录", err)
	}

	// 4. 加入第二个原对象，续扫共享剩余页数，两项任务的扫描结果按原对象分组。
	secondKey, err := service.GenerateUploadObjectKey(7)
	if err != nil {
		t.Fatal(err)
	}
	secondBound := createSession(secondKey)
	secondCandidate := createSession(secondKey)
	execute("UPDATE users SET reserved_storage=reserved_storage+? WHERE id=7", fileSize)
	execute("INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(27,7,?,?,?,?)",
		settings.Provider, settings.Bucket, secondKey, fileSize)
	execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
		expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
		VALUES(28,7,11,27,13,'fixture-second.bin','MULTIPART',?,UNHEX(REPEAT('cd',32)),?,?,2,'UPLOADING',?,UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`,
		secondBound, fileSize, partSize, fileSize)
	beforeSecond, err := store.FindUserUploadTask(ctx, 7, 28)
	if err != nil {
		t.Fatal(err)
	}
	batch := service.NewMultipartUploadSessionCandidateBatchScanner(scanner, store.ListMultipartUploadSessionScanTargets)
	paused, err := batch.ScanObjectPage(ctx, service.MultipartUploadSessionCandidateCursor{}, 2, 2, 1, grace)
	if err != nil || paused.Done || paused.PagesScanned != 1 || len(paused.Objects) != 1 ||
		paused.Objects[0].Scan.SkippedCreation != 2 ||
		paused.Next != (service.MultipartUploadSessionCandidateCursor{ObjectID: 23, UploadID: sessions[1]}) {
		t.Fatal("跨对象扫描未在第一个对象中暂停", err)
	}
	resumed, err := batch.ScanObjectPage(ctx, paused.Next, 2, 2, 2, grace)
	if err != nil || !resumed.Done || resumed.PagesScanned != 2 || len(resumed.Objects) != 2 ||
		resumed.Objects[0].Target != target || resumed.Objects[0].Scan.Scanned != 1 || resumed.Objects[0].Scan.SkippedCreation != 1 ||
		resumed.Objects[1].Target.ObjectID != 27 || resumed.Objects[1].Scan.SkippedBound != 1 ||
		len(resumed.Objects[1].Scan.Candidates) != 1 || resumed.Objects[1].Scan.Candidates[0].UploadID != secondCandidate {
		t.Fatal("跨对象扫描未正确续读并共享剩余预算", err)
	}

	// 5. 用扫描得到的原会话验证只读复核，覆盖空分片、缺失及保护变化。
	rechecker := service.NewMultipartUploadSessionCandidateRechecker(settings.Provider, settings.Bucket,
		client.CheckMultipartUploadSessionExists, store.RecheckMultipartUploadSessionCleanup)
	secondObject := resumed.Objects[1]
	candidate := secondObject.Scan.Candidates[0]
	available, err := rechecker.Recheck(ctx, secondObject.Target, candidate, grace)
	if err != nil || !available.Eligible() {
		t.Fatal("仍存在且超期的未保护会话未通过复核", err)
	}
	withinGrace, err := rechecker.Recheck(ctx, secondObject.Target, candidate, 2*time.Hour)
	if err != nil || !withinGrace.Exists || withinGrace.Database.Facts.GraceElapsed || withinGrace.Eligible() {
		t.Fatal("宽限期变化未被重新检查", err)
	}
	missing := candidate
	missing.UploadID = sessions[0]
	absent, err := rechecker.Recheck(ctx, secondObject.Target, missing, grace)
	if err != nil || absent.Exists || absent.Eligible() {
		t.Fatal("其他对象的会话被误认为存在于原键", err)
	}
	newLease, err := rechecker.Recheck(ctx, target, first.Candidates[0], grace)
	if err != nil || !newLease.Exists || !newLease.Database.Facts.Protection.ActiveCreationLease || newLease.Eligible() {
		t.Fatal("扫描后生效的创建租约未保护原会话", err)
	}
	afterFirst, err := store.FindUserUploadTask(ctx, 7, 19)
	if err != nil || !reflect.DeepEqual(before, afterFirst) {
		t.Fatal("跨对象扫描改变了第一个任务", err)
	}
	afterSecond, err := store.FindUserUploadTask(ctx, 7, 28)
	if err != nil || !reflect.DeepEqual(beforeSecond, afterSecond) {
		t.Fatal("跨对象扫描改变了第二个任务", err)
	}
	// 6. 在存储探测返回之前提交一次真实绑定，再由服务进入账号事务。
	var boundTask *domain.UploadTask
	probeAndBind := func(probeContext context.Context, key, uploadID string) (bool, error) {
		exists, err := client.CheckMultipartUploadSessionExists(probeContext, key, uploadID)
		if err != nil || !exists {
			return exists, err
		}
		bindContext, cancel := context.WithTimeout(probeContext, 3*time.Second)
		defer cancel()
		boundTask, err = store.BindMultipartUploadSession(bindContext, afterFirst, uploadID)
		return exists, err
	}
	bindingRechecker := service.NewMultipartUploadSessionCandidateRechecker(settings.Provider, settings.Bucket,
		probeAndBind, store.RecheckMultipartUploadSessionCleanup)
	nowBound, err := bindingRechecker.Recheck(ctx, target, first.Candidates[0], grace)
	if err != nil || boundTask == nil || !nowBound.Exists || nowBound.Database.TaskStatus != domain.UploadStatusUploading ||
		!nowBound.Database.Facts.Protection.Bound || nowBound.Eligible() {
		t.Fatal("探测期间提交的绑定未被加锁复核读到", nowBound, err)
	}
	afterBound, err := store.FindUserUploadTask(ctx, 7, 19)
	if err != nil || !reflect.DeepEqual(boundTask, afterBound) {
		t.Fatal("单条复核改变了新绑定后的任务", err)
	}
	for _, uploadID := range sessions {
		if _, err := client.ListUploadPartsPage(ctx, key, uploadID, 0, 1); err != nil {
			t.Fatal("只读复核后未能读到原会话", err)
		}
	}
	for _, uploadID := range []string{secondBound, secondCandidate} {
		if _, err := client.ListUploadPartsPage(ctx, secondKey, uploadID, 0, 1); err != nil {
			t.Fatal("跨对象扫描后未能读到第二个对象的会话", err)
		}
	}
	t.Log("两个对象共享会话页数上限，断点续读分组正确，五个合成会话仍可读")
	t.Log("探测先于账号事务，期间提交的真实绑定被锁内复核识别，五个合成会话仍可读")

	// 7. 只允许实际中止命中测试选定的原键与会话，确认同键绑定没有受到影响。
	abortCalls := 0
	aborter := service.NewMultipartUploadSessionCandidateAborter(rechecker, func(callContext context.Context, objectKey, uploadID string) error {
		abortCalls++
		if objectKey != secondKey || uploadID != secondCandidate {
			t.Fatal("实际中止范围偏离隔离夹具的目标会话")
		}
		return client.AbortMultipartUpload(callContext, objectKey, uploadID)
	})
	skipped, err := aborter.AbortCandidate(ctx, target, first.Candidates[0], grace)
	if err != nil || skipped != service.MultipartUploadSessionAbortSkipped || abortCalls != 0 {
		t.Fatal("已绑定会话未跳过", skipped, err)
	}
	accepted, err := aborter.AbortCandidate(ctx, secondObject.Target, candidate, grace)
	if err != nil || accepted != service.MultipartUploadSessionAbortAccepted || abortCalls != 1 {
		t.Fatal("孤儿会话中止未取得存储确认", accepted, err)
	}
	gone, err := aborter.AbortCandidate(ctx, secondObject.Target, candidate, grace)
	if err != nil || gone != service.MultipartUploadSessionAbortMissing || abortCalls != 1 {
		t.Fatal("再次检查缺失会话触发了额外中止", gone, err)
	}
	for _, uploadID := range sessions {
		if _, err := client.ListUploadPartsPage(ctx, key, uploadID, 0, 1); err != nil {
			t.Fatal("另一个原对象的会话受到了影响", err)
		}
	}
	if _, err := client.ListUploadPartsPage(ctx, secondKey, secondBound, 0, 1); err != nil {
		t.Fatal("同键的绑定会话受到了影响", err)
	}
	afterAbort, err := store.FindUserUploadTask(ctx, 7, 28)
	if err != nil || !reflect.DeepEqual(beforeSecond, afterAbort) {
		t.Fatal("中止会话改变了当前绑定任务记录", err)
	}
	t.Log("合成孤儿会话中止获受理，再读确认缺失，同键绑定会话和另三个合成会话仍可读")
}
