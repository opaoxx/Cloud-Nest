package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
)

func TestServerCreateUploadReplacementIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_CREATE_UPLOAD_REPLACEMENT_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_CREATE_UPLOAD_REPLACEMENT_TEST=1 后使用独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 启动实际服务，用合成数据发布一份普通文件作为替换目标。
	fixture := prepareBackupSource(t, ctx)
	t.Logf("替换创建专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	userID := backupFixtureID(t, fixture.login.User.ID)
	parentID := backupFixtureID(t, fixture.login.RootEntryID)
	original := []byte("cloudnest replacement route fixture\n")
	entryID, originalObject := publishBackupSourceFile(t, ctx, fixture.api, store, fixture.bucket, userID, parentID, 0, "replacement-source.bin", original)
	target, err := store.FindUserFileEntry(ctx, userID, entryID)
	if err != nil || target == nil {
		t.Fatal("无法读取合成目标版本", err)
	}
	path := fmt.Sprintf("/api/v1/entries/%d/content-replacement", entryID)
	digest := sha256.Sum256([]byte("replacement declaration fixture"))
	bodyFor := func(revision, size uint64) string {
		return fmt.Sprintf(`{"expected_revision":"%d","size_bytes":%d,"sha256":"%x"}`, revision, size, digest)
	}

	// 2. 通过正式路径核对认证、两种创建模式及同键重试。
	anonymous := fixture.api
	anonymous.accessToken = ""
	var rejected dto.ErrorResponse
	anonymous.request(t, ctx, http.MethodPost, path, "", strings.NewReader("{"), http.StatusUnauthorized, &rejected)
	if rejected.Error.Code != "AUTH_REQUIRED" {
		t.Fatalf("正式路径未先通过授权检查 %+v", rejected)
	}
	const boundary = domain.MaxSingleUploadSizeBytes
	for _, size := range []uint64{boundary, boundary + 1} {
		key := fmt.Sprintf("replacement-route-%d", size)
		var first, replay struct {
			Data      json.RawMessage `json:"data"`
			RequestID string          `json:"request_id"`
		}
		body := bodyFor(target.Revision, size)
		fixture.api.request(t, ctx, http.MethodPost, path, key, strings.NewReader(body), http.StatusCreated, &first)
		var taskID string
		if size == boundary {
			data, err := dto.RestoreCreateUploadReplacementData(first.Data)
			if err != nil || data.Task.Status != "UPLOADING" || data.ContentPath != "/api/v1/uploads/"+data.Task.ID+"/content" {
				t.Fatal("正式路径未交付 SINGLE 内容路径", err)
			}
			taskID = data.Task.ID
		} else {
			data, err := dto.RestoreCreateMultipartUploadReplacementData(first.Data)
			if err != nil || data.Task.Status != "CREATING" || data.PartSizeBytes != boundary || data.PartCount != 2 {
				t.Fatal("正式路径未交付两片计划", err)
			}
			taskID = data.Task.ID
		}
		if err := dto.ValidateCreateUploadReplacementSnapshot(first.Data, backupFixtureID(t, taskID), entryID, target.Revision, size); err != nil {
			t.Fatal("正式响应中的替换绑定和上传声明不符", err)
		}
		var task dto.UploadTaskResponse
		fixture.api.request(t, ctx, http.MethodGet, "/api/v1/uploads/"+taskID, "", nil, http.StatusOK, &task)
		if task.Data.ID != taskID || task.Data.SizeBytes != size || task.Data.Replacement == nil ||
			task.Data.Replacement.EntryID != fmt.Sprint(entryID) || task.Data.Replacement.ExpectedRevision != fmt.Sprint(target.Revision) {
			t.Fatal("既有任务查询未读回替换绑定", task.Data)
		}
		fixture.api.request(t, ctx, http.MethodPost, path, key, strings.NewReader(body), http.StatusCreated, &replay)
		if first.RequestID == "" || replay.RequestID == "" || first.RequestID == replay.RequestID || !bytes.Equal(first.Data, replay.Data) {
			t.Fatal("同键重试未交付原快照和本次请求 ID")
		}
	}

	// 3. 核对共享大小限制、任务数和配额，确认准备阶段保留原内容。
	fixture.api.request(t, ctx, http.MethodPost, path, "replacement-too-large",
		strings.NewReader(bodyFor(target.Revision, domain.DefaultMaxUploadSizeBytes+1)), http.StatusRequestEntityTooLarge, &rejected)
	if rejected.Error.Code != "FILE_TOO_LARGE" || rejected.Error.Details["max_size_bytes"] != float64(domain.DefaultMaxUploadSizeBytes) {
		t.Fatalf("正式替换路径未沿用普通上传大小限制 %+v", rejected)
	}
	var used, reserved, replacementTasks, replacementReceipts, history, replacements uint64
	err = fixture.pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,
		(SELECT COUNT(*) FROM upload_tasks WHERE user_id=users.id AND replacement_entry_id=?),
		(SELECT COUNT(*) FROM idempotency_records WHERE user_id=users.id AND operation=?),
		(SELECT COUNT(*) FROM file_versions WHERE user_id=users.id AND entry_id=?),
		(SELECT COUNT(*) FROM change_events WHERE user_id=users.id AND event_type='FILE_CONTENT_REPLACED')
		FROM users WHERE id=?`, entryID, domain.CreateUploadReplacementOperation, entryID, userID).
		Scan(&used, &reserved, &replacementTasks, &replacementReceipts, &history, &replacements)
	if err != nil || used != uint64(len(original)) || reserved != 2*boundary+1 ||
		replacementTasks != 2 || replacementReceipts != 2 || history != 0 || replacements != 0 {
		t.Fatalf("正式创建的任务、回执、历史或配额不符，已用 %d，预占 %d，任务 %d，回执 %d，历史 %d，替换事件 %d，错误 %v",
			used, reserved, replacementTasks, replacementReceipts, history, replacements, err)
	}
	after, err := store.FindUserFileEntry(ctx, userID, entryID)
	if err != nil || !reflect.DeepEqual(target, after) {
		t.Fatal("准备阶段改变了原目录项", err)
	}
	content, err := store.FindUserFileEntryContent(ctx, userID, entryID)
	if err != nil || content == nil || content.ObjectID == nil || *content.ObjectID != originalObject.ID {
		t.Fatal("准备阶段改变了原对象引用", err)
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务退出失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	t.Logf("正式路径替换目标 %d、版本 %d，2 个任务和回执，原内容已用 %d B，新内容预占 %d B", entryID, target.Revision, used, reserved)
}
