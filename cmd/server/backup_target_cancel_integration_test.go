package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerBackupTargetCancelIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_BACKUP_SOURCE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_BACKUP_SOURCE_TEST=1 后验证恢复目标的取消收尾")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 对齐恢复后的暂存定位，启动使用隔离目标库和目标桶的真实服务。
	fixture, pending := prepareBackupTargetPendingUpload(t, ctx)
	pool, target := fixture.database.pool, fixture.targetBucket
	staged := snapshotBackupTargetObjects(t, ctx, pool, pending.UserID)[pending.StagedObjectID]
	if err := alignBackupTargetStagingBucket(ctx, pool, target, pending, staged); err != nil {
		t.Fatal(err)
	}
	before := snapshotBackupTargetObjects(t, ctx, pool, pending.UserID)
	if before[staged.ID].Bucket != target.name || os.Getenv("CLOUDNEST_S3_BUCKET") != target.name {
		t.Fatal("隔离目标服务未绑定到已对齐的目标桶")
	}
	api, stop := startBackupFixtureServer(t, ctx, fixture.database.name)
	login := loginBackupFixture(t, ctx, &api, os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
	if login.User.ID != fixture.source.login.User.ID || login.RootEntryID != fixture.source.login.RootEntryID ||
		login.Session.ID == fixture.source.login.Session.ID || login.UsedBytes != 10 || login.ReservedBytes != 3 {
		t.Fatal("恢复后的账号、独立会话或初始配额不符")
	}
	t.Logf("恢复后的后端 %s 使用目标库 %s，合成账号重新登录，已用 10 B、预占 3 B", api.baseURL, fixture.database.name)

	// 2. 调用一次正式取消入口，按任务 ID 等待真实 worker 写入终态。
	taskID := strconv.FormatUint(pending.ID, 10)
	path := "/api/v1/uploads/" + taskID
	var accepted dto.UploadTaskResponse
	api.request(t, ctx, http.MethodPost, path+"/cancel", "", nil, http.StatusAccepted, &accepted)
	if accepted.Data.ID != taskID || accepted.Data.Status != "CANCELLING" ||
		accepted.Data.ResultEntryID != nil || accepted.Data.CompletedAt != nil {
		t.Fatal("正式取消没有返回该任务的受理状态")
	}
	finished := awaitBackupTargetCancellation(t, ctx, pool, pending)

	// 3. 对比公开结果、数据库与目标桶版本，确认预占已返还且三个发布文件仍在。
	var status dto.UploadTaskResponse
	api.request(t, ctx, http.MethodGet, path, "", nil, http.StatusOK, &status)
	if status.Data.ID != accepted.Data.ID || status.Data.Status != "CANCELLED" || status.Data.ResultEntryID != nil ||
		status.Data.CompletedAt == nil || !status.Data.CompletedAt.Equal(*finished.CompletedAt) {
		t.Fatal("公开取消终态与数据库不一致")
	}
	var profile dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	var used, reserved, files uint64
	if err := pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,
		(SELECT COUNT(*) FROM file_entries WHERE user_id=users.id AND entry_type='FILE') FROM users WHERE id=?`, pending.UserID).
		Scan(&used, &reserved, &files); err != nil {
		t.Fatal(err)
	}
	if used != 10 || reserved != 0 || files != 3 || profile.Data.UsedBytes != used || profile.Data.ReservedBytes != reserved {
		t.Fatalf("取消后账目或文件数不符，数据库 %d/%d，API %d/%d，文件 %d", used, reserved, profile.Data.UsedBytes, profile.Data.ReservedBytes, files)
	}
	after := snapshotBackupTargetObjects(t, ctx, pool, pending.UserID)
	orphan := after[staged.ID]
	if orphan.Bucket != target.name || orphan.ObjectKey != staged.ObjectKey || orphan.Status != domain.StoredObjectStatusOrphaned ||
		orphan.StorageVersionID != nil || orphan.StorageETag != nil || orphan.VerifiedSHA256 != nil || orphan.UnreferencedAt == nil ||
		orphan.GCAfter == nil || !orphan.GCAfter.Equal(orphan.UnreferencedAt.Add(7*24*time.Hour)) {
		t.Fatal("空暂存对象未正确进入目标端的回收等待")
	}
	for _, location := range fixture.mapping.Objects {
		if !reflect.DeepEqual(after[location.ObjectID], before[location.ObjectID]) {
			t.Fatal("取消修改了已发布对象", location.ObjectID)
		}
	}
	verifyBackupTargetVersions(t, ctx, target, fixture.mapping.Objects)
	if !reflect.DeepEqual(readBackupUploadTask(t, ctx, fixture.source.pool, pending.UserID, pending.ID), pending) {
		t.Fatal("目标取消影响了源端任务")
	}
	logs, err := stop()
	if err != nil {
		t.Fatal(err)
	}
	verifyBackupTargetCancellationRound(t, logs)
	t.Logf("任务 %d：202 CANCELLING → 200 CANCELLED，完成于 %s，返还 3 B；已用 10 B、预占 0 B，保留三个实际 MinIO 版本",
		pending.ID, finished.CompletedAt.Format(time.RFC3339Nano))
}

func awaitBackupTargetCancellation(t *testing.T, ctx context.Context, pool *sql.DB, pending *domain.UploadTask) *domain.UploadTask {
	t.Helper()
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	waitContext, cancel := context.WithTimeout(ctx, uploadCancellationInterval+10*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		task, err := store.FindUserUploadTask(waitContext, pending.UserID, pending.ID)
		if err != nil || task == nil {
			t.Fatal("等待目标端后台收尾失败", err)
		}
		if task.Status == domain.UploadStatusCancelled {
			if task.CompletedAt == nil || task.ReservedBytes != 0 || task.StagedObjectID != pending.StagedObjectID ||
				task.ResultEntryID != nil || task.PublishedObjectID != nil {
				t.Fatal("目标端取消终态或预占数据不符")
			}
			return task
		}
		if task.Status != domain.UploadStatusCancelling || task.ReservedBytes != pending.ReservedBytes {
			t.Fatal("后台收尾前的任务状态或预占不符", task.Status, task.ReservedBytes)
		}
		select {
		case <-waitContext.Done():
			t.Fatal("目标端 worker 未在等待期限内完成取消", waitContext.Err())
		case <-ticker.C:
		}
	}
}

func verifyBackupTargetCancellationRound(t *testing.T, logs string) {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "上传取消轮次结束") {
			continue
		}
		var summary struct {
			Scanned         int `json:"scanned"`
			Cancelled       int `json:"cancelled"`
			Retries         int `json:"retries_scheduled"`
			CandidateErrors int `json:"candidate_errors"`
		}
		if err := json.Unmarshal([]byte(line), &summary); err != nil || summary.Scanned != 1 || summary.Cancelled != 1 ||
			summary.Retries != 0 || summary.CandidateErrors != 0 {
			t.Fatal("目标端取消 worker 汇报不符", line, err)
		}
		return
	}
	t.Fatal("目标端取消 worker 没有输出成功轮次")
}
