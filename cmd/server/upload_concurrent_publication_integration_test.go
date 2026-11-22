package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

// concurrentPublicationReader 在关闭真实对象流后，让两个校验处理器都到达发布前。
type concurrentPublicationReader struct {
	io.ReadCloser
	awaitPublication func() error
}

func (reader *concurrentPublicationReader) Close() error {
	if err := reader.ReadCloser.Close(); err != nil {
		return err
	}
	return reader.awaitPublication()
}

func TestServerConcurrentMultipartPublicationIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_CONCURRENT_PUBLICATION_TEST") != "1" {
		t.Skip("显式启用后创建专用 MySQL 与 MinIO 验证并发发布")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 在专用版本桶写入两份合成内容，创建对应的已合并待校验任务。
	mysqlID := startIsolatedWebMySQL(t, ctx)
	minioID := startIsolatedWebMinIO(t, ctx)
	t.Logf("并发发布专用 MySQL %s，专用 MinIO %s", mysqlID[:12], minioID[:12])
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
	execute := func(statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal(err)
		}
	}
	const userID, partSize, fileSize = 7, 16 << 20, (16 << 20) + 1
	const requestedName = "并发发布.bin"
	execute("INSERT INTO users(id,username,display_name,password_hash,reserved_storage) VALUES(7,'publication_fixture','并发发布账号','fixture_hash',?)", 2*fileSize)
	execute("INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(11,7,'28ee758a-0727-42de-ae7d-3bb33155e148','fixture','WEB')")
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR')")

	content := bytes.Repeat([]byte{0x71}, fileSize)
	digest := sha256.Sum256(content)
	versions := make(map[uint64]string, 2)
	for _, taskID := range []uint64{19, 20} {
		key := fmt.Sprintf("b4/concurrent-publication-%d.bin", taskID)
		object, err := bucket.client.PutObject(ctx, bucket.name, key, bytes.NewReader(content), fileSize, minio.PutObjectOptions{DisableMultipart: true})
		if err != nil || object.VersionID == "" || object.ETag == "" {
			t.Fatal("未取得合成对象的版本和 ETag", minio.ToErrorResponse(err).Code)
		}
		versions[taskID] = object.VersionID
		execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,storage_version_id,storage_etag,file_size,status)
			VALUES(?,7,?,?,?,?,?,?,'VERIFYING')`, taskID, settings.Provider, bucket.name, key, object.VersionID, object.ETag, fileSize)
		execute(`INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,storage_upload_id,
			expected_sha256,file_size,part_size,part_count,status,reserved_bytes,expires_at)
			VALUES(?,7,11,?,13,?,'MULTIPART',?,?,?,?,2,'VERIFYING',?,UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`,
			taskID, taskID, requestedName, fmt.Sprintf("fixture-session-%d", taskID), digest[:], fileSize, partSize, fileSize)
	}
	candidates, err := store.ListMultipartUploadVerificationCandidates(ctx, 2)
	if err != nil || len(candidates) != 2 {
		t.Fatal("未找到两个独立的待校验任务", candidates, err)
	}

	// 2. 让两个真实处理器都读完原版本，在发布前共同等待。
	workContext, stopWork := context.WithTimeout(ctx, 20*time.Second)
	var workers sync.WaitGroup
	defer func() {
		stopWork()
		workers.Wait()
	}()
	arrived := make(chan uint64, len(candidates))
	release := make(chan struct{})
	open := func(readContext context.Context, object *domain.StoredObject) (io.ReadCloser, error) {
		stream, err := client.OpenUploadVerificationObject(readContext, object)
		if err != nil {
			return nil, err
		}
		return &concurrentPublicationReader{ReadCloser: stream, awaitPublication: func() error {
			arrived <- object.ID
			select {
			case <-release:
				return nil
			case <-readContext.Done():
				return readContext.Err()
			}
		}}, nil
	}
	type result struct {
		task *domain.UploadTask
		err  error
	}
	results := make(chan result, len(candidates))
	for _, candidate := range candidates {
		processor := newServerMultipartUploadVerificationProcessor(store, open)
		workers.Go(func() {
			task, err := processor.Process(workContext, candidate, fmt.Sprintf("fixture-worker-%d", candidate.TaskID))
			results <- result{task: task, err: err}
		})
	}
	reached := make(map[uint64]bool, len(candidates))
	for range candidates {
		select {
		case objectID := <-arrived:
			if reached[objectID] || versions[objectID] == "" {
				t.Fatal("发布前没有读完两份独立的暂存对象", objectID)
			}
			reached[objectID] = true
		case early := <-results:
			t.Fatal("尚未放行发布，处理器已返回", early.task, early.err)
		case <-workContext.Done():
			t.Fatal("两个处理器未及时达到发布前", workContext.Err())
		}
	}
	var activeLeases, readyBefore int
	if err := pool.QueryRowContext(workContext, `SELECT
		(SELECT COUNT(*) FROM upload_tasks WHERE user_id=? AND status='VERIFYING' AND lease_owner IS NOT NULL),
		(SELECT COUNT(*) FROM stored_objects WHERE owner_user_id=? AND status='READY')`, userID, userID).
		Scan(&activeLeases, &readyBefore); err != nil || activeLeases != 2 || readyBefore != 0 {
		t.Fatal("放行前应有两个已领取任务且没有 READY 对象", activeLeases, readyBefore, err)
	}
	close(release)

	// 3. 回读两项发布及各自的计费，确认冗余暂存对象只进入回收期。
	var publishedID uint64
	entryIDs := make(map[uint64]bool, len(candidates))
	for range candidates {
		select {
		case finished := <-results:
			task := finished.task
			if finished.err != nil || task == nil || task.Status != domain.UploadStatusCompleted || task.PublishedObjectID == nil ||
				task.ResultEntryID == nil || task.ReservedBytes != 0 || task.LeaseOwner != nil {
				t.Fatal("并发校验未提交发布结果", task, finished.err)
			}
			if publishedID == 0 {
				publishedID = *task.PublishedObjectID
			}
			if *task.PublishedObjectID != publishedID || entryIDs[*task.ResultEntryID] {
				t.Fatal("两项上传未取得各自条目及同一发布对象")
			}
			entryIDs[*task.ResultEntryID] = true
		case <-workContext.Done():
			t.Fatal("发布未及时结束", workContext.Err())
		}
	}
	names := make(map[string]bool, len(entryIDs))
	for entryID := range entryIDs {
		var name string
		var objectID, size uint64
		if err := pool.QueryRowContext(ctx, "SELECT name,object_id,file_size FROM file_entries WHERE id=? AND user_id=? AND entry_type='FILE' AND status='ACTIVE'", entryID, userID).
			Scan(&name, &objectID, &size); err != nil || objectID != publishedID || size != fileSize {
			t.Fatal("活动文件引用或大小不符", entryID, objectID, size, err)
		}
		names[name] = true
	}
	if !names[requestedName] || !names["并发发布 (1).bin"] {
		t.Fatal("并发发布未分配原名和重名后缀", names)
	}
	for objectID, version := range versions {
		object, err := store.FindUserStoredObject(ctx, userID, objectID)
		if err != nil || object == nil || object.StorageVersionID == nil || *object.StorageVersionID != version {
			t.Fatal("发布改变了原对象的存储版本", objectID, err)
		}
		if objectID == publishedID {
			if object.Status != domain.StoredObjectStatusReady || object.VerifiedSHA256 == nil || *object.VerifiedSHA256 != digest ||
				object.UnreferencedAt != nil || object.GCAfter != nil {
				t.Fatal("共享对象未保留已验证的 READY 状态", objectID)
			}
		} else if object.Status != domain.StoredObjectStatusOrphaned || object.UnreferencedAt == nil || object.GCAfter == nil ||
			!object.GCAfter.Equal(object.UnreferencedAt.Add(7*24*time.Hour)) {
			t.Fatal("冗余版本未进入七天回收宽限期", objectID)
		}
	}
	var used, reserved uint64
	var ready, fileCount int
	if err := pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,
		(SELECT COUNT(*) FROM stored_objects WHERE owner_user_id=u.id AND status='READY'),
		(SELECT COUNT(*) FROM file_entries WHERE user_id=u.id AND entry_type='FILE' AND status='ACTIVE') FROM users u WHERE id=?`, userID).
		Scan(&used, &reserved, &ready, &fileCount); err != nil || used != 2*fileSize || reserved != 0 || ready != 1 || fileCount != 2 {
		t.Fatal("并发发布后的配额或计数不符", used, reserved, ready, fileCount, err)
	}
	t.Logf("两条任务完成，两个活动条目共用 READY 对象 %d，已用 %d B，预占 %d B", publishedID, used, reserved)
}
