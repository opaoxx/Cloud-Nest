package main

import (
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

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerCreateUploadByModeIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_CREATE_UPLOAD_BY_MODE_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_CREATE_UPLOAD_BY_MODE_TEST=1 后使用独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("双模式创建专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	const path = "/api/v1/uploads"
	singleSize := domain.MaxSingleUploadSizeBytes
	multipartSize := singleSize + 1
	rootID := fixture.login.RootEntryID

	// 1. 验证正式路由仍接受 16 MiB 的 SINGLE，并能重放旧版保存的快照。
	singleBody := fmt.Sprintf(`{"parent_id":%q,"name":"single-boundary.bin","size_bytes":%d}`, rootID, singleSize)
	single := requestServerCreateUpload(t, fixture.api.baseURL, fixture.api.accessToken,
		"single-boundary", singleBody, http.StatusCreated, "", "")
	if single.Data.UploadMode != "SINGLE" || single.Data.Task.Status != "UPLOADING" || single.Data.Task.SizeBytes != singleSize ||
		single.Data.ContentPath != path+"/"+single.Data.Task.ID+"/content" {
		t.Fatal("16 MiB 的 SINGLE 创建响应不符", single.Data)
	}
	legacy := createLegacyUploadSnapshot(t, ctx, fixture)
	legacyBody := fmt.Sprintf(`{"parent_id":%q,"name":"legacy-single.bin","size_bytes":0}`, rootID)
	replayedSingle := requestServerCreateUpload(t, fixture.api.baseURL, fixture.api.accessToken,
		"legacy-single", legacyBody, http.StatusCreated, "", "")
	if !reflect.DeepEqual(*legacy, replayedSingle.Data) {
		t.Fatal("新版路由未重放旧版 SINGLE 创建快照")
	}

	// 2. 创建两片任务，核对初始快照及后台绑定的原始存储会话。
	digest := sha256.Sum256(make([]byte, multipartSize))
	const key = "multipart-boundary"
	multipartBody := fmt.Sprintf(`{"parent_id":%q,"name":"multipart-boundary.bin","size_bytes":%d,"sha256":"%x"}`,
		rootID, multipartSize, digest)
	var created, replayed struct {
		Data      json.RawMessage `json:"data"`
		RequestID string          `json:"request_id"`
	}
	fixture.api.request(t, ctx, http.MethodPost, path, key, strings.NewReader(multipartBody), http.StatusCreated, &created)
	multipart, err := dto.RestoreCreateMultipartUploadData(created.Data)
	if err != nil || created.RequestID == "" || multipart.Task.SizeBytes != multipartSize ||
		multipart.Task.ParentID != rootID || multipart.PartSizeBytes != singleSize || multipart.PartCount != 2 {
		t.Fatal("Multipart 创建快照未交付固定的两片计划", err)
	}
	taskPath := path + "/" + multipart.Task.ID
	task := waitForCreateUploadInitialization(t, ctx, fixture.api, taskPath)
	if task.ID != multipart.Task.ID || task.UploadMode != "MULTIPART" || task.SizeBytes != multipartSize ||
		!task.ExpiresAt.Equal(multipart.ExpiresAt) || task.ResultEntryID != nil {
		t.Fatal("初始化后的任务与原创建计划不符", task)
	}
	var storageUploadID, objectKey string
	if err := fixture.pool.QueryRowContext(ctx, `SELECT task.storage_upload_id, obj.object_key
		FROM upload_tasks task JOIN stored_objects obj ON obj.id=task.staged_object_id
		WHERE task.id=? AND task.user_id=? AND obj.bucket=? AND task.part_size=? AND task.part_count=2`,
		task.ID, fixture.login.User.ID, fixture.bucket.name, singleSize).Scan(&storageUploadID, &objectKey); err != nil ||
		storageUploadID == "" || objectKey == "" {
		t.Fatal("任务未保存预期计划并绑定专用桶会话", err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		core := minio.Core{Client: fixture.bucket.client}
		if err := core.AbortMultipartUpload(cleanup, fixture.bucket.name, objectKey, storageUploadID); err != nil {
			t.Error("隔离分片会话清理失败", minio.ToErrorResponse(err).Code)
		}
	})
	var receipts dto.UploadPartsResponse
	fixture.api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &receipts)
	if receipts.Data.PartSizeBytes != singleSize || receipts.Data.PartCount != 2 ||
		receipts.Data.Parts == nil || len(receipts.Data.Parts) != 0 {
		t.Fatal("初始化后的空分片清单不符", receipts.Data)
	}

	// 3. 检查初始化后的同键重放、超限拒绝、摘要要求及配额预占总数。
	fixture.api.request(t, ctx, http.MethodPost, path, key, strings.NewReader(multipartBody), http.StatusCreated, &replayed)
	replayedMultipart, err := dto.RestoreCreateMultipartUploadData(replayed.Data)
	if err != nil || replayed.RequestID == "" || replayed.RequestID == created.RequestID || !reflect.DeepEqual(multipart, replayedMultipart) {
		t.Fatal("Multipart 同键请求未交付原始 CREATING 快照", err)
	}
	requestServerCreateUpload(t, fixture.api.baseURL, fixture.api.accessToken, "missing-file-digest", fmt.Sprintf(
		`{"parent_id":%q,"name":"missing-digest.bin","size_bytes":%d}`, rootID, multipartSize),
		http.StatusBadRequest, "INVALID_ARGUMENT", "sha256")
	requestServerCreateUpload(t, fixture.api.baseURL, fixture.api.accessToken, "file-too-large", fmt.Sprintf(
		`{"parent_id":%q,"name":"oversize.bin","size_bytes":%d,"sha256":"%x"}`, rootID, domain.DefaultMaxUploadSizeBytes+1, digest),
		http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "size_bytes")
	var used, reserved, taskCount, snapshotCount uint64
	if err := fixture.pool.QueryRowContext(ctx, `SELECT used_storage,reserved_storage,
		(SELECT COUNT(*) FROM upload_tasks WHERE user_id=users.id),
		(SELECT COUNT(*) FROM idempotency_records WHERE user_id=users.id AND operation=?)
		FROM users WHERE id=?`, domain.CreateUploadOperation, fixture.login.User.ID).
		Scan(&used, &reserved, &taskCount, &snapshotCount); err != nil ||
		used != 0 || reserved != singleSize+multipartSize || taskCount != 3 || snapshotCount != taskCount {
		t.Fatal("重放或拒绝后上传记录和预占不符", err, used, reserved, taskCount, snapshotCount)
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务退出失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	t.Logf("SINGLE %d B 直接 UPLOADING，MULTIPART %d B 为 %d 片、每片上限 %d B，任务 %s 从 CREATING 到 UPLOADING；历史及本轮快照可重放，超限返回 413，已用 %d B、预占 %d B",
		singleSize, multipartSize, multipart.PartCount, multipart.PartSizeBytes, task.ID, used, reserved)
}

func createLegacyUploadSnapshot(t *testing.T, ctx context.Context, fixture backupSourceFixture) *dto.CreateUploadData {
	t.Helper()
	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	// 1. 在隔离库用旧版 SINGLE 构造与编解码保存一份历史幂等结果。
	creator := service.NewIdempotentCreateUploadService("local_minio", fixture.bucket.name,
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentCreateUploadTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *repository.Store) error {
				return action(txContext, txStore)
			})
		}, func(task *domain.UploadTask) (json.RawMessage, error) {
			data, err := dto.NewCreateUploadData(task)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		}, func(body json.RawMessage) error {
			_, err := dto.RestoreCreateUploadData(body)
			return err
		})
	actor := domain.Actor{
		UserID: backupFixtureID(t, fixture.login.User.ID), DeviceID: backupFixtureID(t, fixture.login.Device.ID),
		SessionID: backupFixtureID(t, fixture.login.Session.ID),
	}
	body, err := creator.Create(ctx, actor, domain.CreateUploadInput{
		ParentID: backupFixtureID(t, fixture.login.RootEntryID), Name: "legacy-single.bin",
	}, "legacy-single")
	if err != nil {
		t.Fatal("隔离库生成旧版 SINGLE 创建记录失败", err)
	}
	// 2. 保留旧版快照供新版入口逐字段核对。
	data, err := dto.RestoreCreateUploadData(body)
	if err != nil {
		t.Fatal("旧版创建快照不可读", err)
	}
	return data
}

func waitForCreateUploadInitialization(t *testing.T, ctx context.Context, api backupSourceClient, taskPath string) dto.UploadTaskData {
	t.Helper()
	waitContext, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	poll := time.NewTicker(200 * time.Millisecond)
	defer poll.Stop()
	for {
		var result dto.UploadTaskResponse
		api.request(t, waitContext, http.MethodGet, taskPath, "", nil, http.StatusOK, &result)
		if result.Data.Status == "UPLOADING" {
			return result.Data
		}
		if result.Data.Status != "CREATING" {
			t.Fatal("分片初始化进入意外状态", result.Data.Status, result.Data.LastError)
		}
		select {
		case <-waitContext.Done():
			t.Fatal("分片初始化 worker 未按期绑定会话")
		case <-poll.C:
		}
	}
}
