package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerConcurrentUploadReplacementPublicationIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_CONCURRENT_REPLACEMENT_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_CONCURRENT_REPLACEMENT_TEST=1 后使用独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 两台设备分别创建绑定同一版本的任务。
	fixture := prepareReplacementPublicationFixture(t, ctx)
	t.Logf("并发替换发布专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	secondAPI := fixture.api
	second := loginBackupFixture(t, ctx, &secondAPI, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
	if second.User.ID != fixture.login.User.ID || second.Device.ID == fixture.login.Device.ID || second.Session.ID == fixture.login.Session.ID {
		t.Fatal("并发夹具需要同账号的独立设备和会话")
	}
	type replacementAttempt struct {
		actor   domain.Actor
		content []byte
	}
	attempts := make(map[uint64]replacementAttempt, 2)
	var totalReserved uint64
	for index, device := range []struct {
		api     backupSourceClient
		login   dto.LoginData
		content []byte
	}{
		{fixture.api, fixture.login, []byte("first device replacement\n")},
		{secondAPI, second, []byte("second device replacement has a different body\n")},
	} {
		var created dto.CreateUploadResponse
		digest := sha256.Sum256(device.content)
		body := fmt.Sprintf(`{"expected_revision":"%d","size_bytes":%d,"sha256":"%x"}`, fixture.target.Revision, len(device.content), digest)
		device.api.request(t, ctx, http.MethodPost, fixture.entryPath+"/content-replacement", fmt.Sprintf("concurrent-replacement-%d", index+1),
			strings.NewReader(body), http.StatusCreated, &created)
		task := created.Data.Task
		taskID := backupFixtureID(t, task.ID)
		if _, reused := attempts[taskID]; reused || task.Status != "UPLOADING" || task.Replacement == nil ||
			task.Replacement.EntryID != fmt.Sprint(fixture.target.ID) || task.Replacement.ExpectedRevision != fmt.Sprint(fixture.target.Revision) {
			t.Fatal("两个设备没有取得绑定同一原版本的独立任务")
		}
		attempts[taskID] = replacementAttempt{actor: domain.Actor{
			UserID: fixture.userID, DeviceID: backupFixtureID(t, device.login.Device.ID), SessionID: backupFixtureID(t, device.login.Session.ID),
		}, content: device.content}
		totalReserved += uint64(len(device.content))
	}
	fixture.checkQuota(t, ctx, uint64(len(fixture.original)), totalReserved)

	// 2. 停止自动轮询，走正式接收 service，然后同步放行两个校验处理器。
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务退出失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	settings, err := config.LoadStorageConfig()
	if err != nil || settings.Bucket != fixture.bucket.name {
		t.Fatal("接收测试须绑定本轮专用桶", err)
	}
	storageClient, err := storage.NewClient(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storageClient.Close)
	files, err := service.NewUploadContentFileService(t.TempDir(), service.NewUploadTempSpace())
	if err != nil {
		t.Fatal(err)
	}
	preparation := service.NewUploadContentPreparationService(settings.Provider, settings.Bucket,
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.UploadContentPreparationTransaction) error) error {
			return fixture.store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		})
	receiver := service.NewUploadContentReceiveService(files, settings.Provider, settings.Bucket, preparation.Prepare, storageClient.PutUploadContent,
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.UploadContentReceiveTransaction) error) error {
			return fixture.store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		})
	for taskID, attempt := range attempts {
		task, err := fixture.store.FindUserUploadTask(ctx, fixture.userID, taskID)
		if err != nil || task.DeviceID != attempt.actor.DeviceID {
			t.Fatal("创建任务未保存对应设备", err)
		}
		received, err := receiver.Receive(ctx, attempt.actor, task, bytes.NewReader(attempt.content))
		if err != nil || received.Status != domain.UploadStatusVerifying {
			t.Fatal("真实接收流程未交给待校验阶段", err)
		}
	}
	candidates, err := fixture.store.ListUploadVerificationCandidates(ctx, 2)
	if err != nil || len(candidates) != 2 {
		t.Fatal("未找到两个独立待校验的替换任务", err)
	}

	workContext, stopWork := context.WithTimeout(ctx, 20*time.Second)
	var workers sync.WaitGroup
	defer func() {
		stopWork()
		workers.Wait()
	}()
	arrived := make(chan uint64, len(candidates))
	release := make(chan struct{})
	processor := newServerUploadVerificationProcessor(fixture.store, fixture.store.ClaimUploadVerificationLease,
		func(ctx context.Context, object *domain.StoredObject) (io.ReadCloser, error) {
			stream, err := storageClient.OpenUploadVerificationObject(ctx, object)
			if err != nil {
				return nil, err
			}
			return &concurrentPublicationReader{ReadCloser: stream, awaitPublication: func() error {
				arrived <- object.ID
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}}, nil
		}, nil)
	type result struct {
		taskID uint64
		task   *domain.UploadTask
		err    error
	}
	results := make(chan result, len(candidates))
	pendingObjects := make(map[uint64]bool, len(candidates))
	for _, candidate := range candidates {
		if _, exists := attempts[candidate.TaskID]; !exists || pendingObjects[candidate.StagedObjectID] {
			t.Fatal("校验候选未绑定两份独立暂存对象")
		}
		pendingObjects[candidate.StagedObjectID] = true
		workers.Go(func() {
			task, err := processor.Process(workContext, candidate, fmt.Sprintf("fixture-replacement-%d", candidate.TaskID))
			results <- result{taskID: candidate.TaskID, task: task, err: err}
		})
	}
	for range candidates {
		select {
		case objectID := <-arrived:
			if !pendingObjects[objectID] {
				t.Fatal("发布前未读回两个独立的暂存对象")
			}
			delete(pendingObjects, objectID)
		case early := <-results:
			t.Fatal("同时放行前已有处理器返回", early.taskID, early.err)
		case <-workContext.Done():
			t.Fatal("两个校验处理器未同时到达发布前", workContext.Err())
		}
	}
	current, err := fixture.store.FindUserFileEntry(ctx, fixture.userID, fixture.target.ID)
	if err != nil || current.Revision != fixture.target.Revision {
		t.Fatal("放行前原版本已经改变", err)
	}
	close(release)

	// 3. 核对只有一个任务发布，另一个保存冲突版本且只释放自己的预占。
	var winner, loser *domain.UploadTask
	newRevision := fixture.target.Revision + 1
	for range candidates {
		select {
		case finished := <-results:
			if finished.err == nil {
				if winner != nil || finished.task == nil || finished.task.ID != finished.taskID {
					t.Fatal("并发替换未产生唯一的成功结果")
				}
				winner = finished.task
				continue
			}
			var conflict *domain.EntryRevisionConflictError
			if loser != nil || finished.task != nil || !errors.As(finished.err, &conflict) || conflict.CurrentRevision != newRevision {
				t.Fatal("失败处理器未返回实际冲突版本", finished.taskID, finished.err)
			}
			loser, err = fixture.store.FindUserUploadTask(ctx, fixture.userID, finished.taskID)
			if err != nil {
				t.Fatal(err)
			}
		case <-workContext.Done():
			t.Fatal("并发发布未在期限内结束", workContext.Err())
		}
	}
	if winner == nil || loser == nil || winner.ID == loser.ID ||
		winner.Status != domain.UploadStatusCompleted || winner.ResultEntryID == nil || *winner.ResultEntryID != fixture.target.ID ||
		winner.PublishedObjectID == nil || winner.ReservedBytes != 0 || winner.ReplacementPublishedRevision == nil || *winner.ReplacementPublishedRevision != newRevision ||
		loser.Status != domain.UploadStatusFailed || loser.ReservedBytes != 0 || loser.ResultEntryID != nil || loser.PublishedObjectID != nil ||
		loser.ReplacementPublishedRevision != nil || loser.ReplacementConflictingRevision == nil || *loser.ReplacementConflictingRevision != newRevision ||
		loser.LastErrorCode == nil || *loser.LastErrorCode != domain.UploadRevisionConflictCode {
		t.Fatal("成功与冲突终态的版本、引用或预占不符")
	}
	failedData, err := dto.NewUploadTaskData(loser)
	if err != nil || failedData.LastError == nil || failedData.LastError.Code != domain.UploadRevisionConflictCode ||
		failedData.Replacement == nil || failedData.Replacement.ConflictingRevision == nil || *failedData.Replacement.ConflictingRevision != fmt.Sprint(newRevision) {
		t.Fatal("冲突终态的公开字段不符", err)
	}
	losingObject, err := fixture.store.FindUserStoredObject(ctx, fixture.userID, loser.StagedObjectID)
	if err != nil || losingObject.Status != domain.StoredObjectStatusOrphaned || losingObject.UnreferencedAt == nil ||
		losingObject.GCAfter == nil || !losingObject.GCAfter.Equal(losingObject.UnreferencedAt.Add(7*24*time.Hour)) {
		t.Fatal("未发布的暂存对象未进入七天回收宽限期", err)
	}

	var sequence, revision, currentObject, historicalObject, histories, events uint64
	var eventDigest string
	err = fixture.pool.QueryRowContext(ctx, `SELECT u.change_seq,e.revision,e.object_id,h.object_id,
		(SELECT COUNT(*) FROM file_versions WHERE user_id=u.id),
		(SELECT COUNT(*) FROM change_events WHERE user_id=u.id AND event_type='FILE_CONTENT_REPLACED'),
		JSON_UNQUOTE(JSON_EXTRACT(event.payload,'$.sha256'))
		FROM users u JOIN file_entries e ON e.user_id=u.id JOIN file_versions h ON h.user_id=u.id AND h.entry_id=e.id AND h.revision=?
		JOIN change_events event ON event.user_id=u.id AND event.event_type='FILE_CONTENT_REPLACED'
		WHERE u.id=? AND e.id=?`, fixture.target.Revision, fixture.userID, fixture.target.ID).
		Scan(&sequence, &revision, &currentObject, &historicalObject, &histories, &events, &eventDigest)
	winnerBody := attempts[winner.ID].content
	winnerDigest := sha256.Sum256(winnerBody)
	if err != nil || sequence != fixture.baselineSequence+1 || revision != newRevision || currentObject != *winner.PublishedObjectID ||
		historicalObject != fixture.originalObject.ID || histories != 1 || events != 1 || eventDigest != fmt.Sprintf("%x", winnerDigest) {
		t.Fatal("并发替换的版本、历史或唯一变更事件不符", err)
	}
	fixture.checkQuota(t, ctx, uint64(len(fixture.original)+len(winnerBody)), 0)
	t.Logf("原版本 %d，两任务同时到达发布；任务 %d 发布版本 %d，已用 %d B；任务 %d 冲突版本 %d，预占 0 B，历史和替换事件各一条",
		fixture.target.Revision, winner.ID, newRevision, len(fixture.original)+len(winnerBody), loser.ID, *loser.ReplacementConflictingRevision)
}
