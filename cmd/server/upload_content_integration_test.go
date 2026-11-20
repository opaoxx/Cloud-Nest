package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

type serverUploadContentBucket struct {
	client *minio.Client
	name   string
}

// prepareServerUploadContentBucket 创建只属于本次验证的桶，使用独立客户端读回并清理。
func prepareServerUploadContentBucket(t *testing.T, ctx context.Context) *serverUploadContentBucket {
	t.Helper()
	settings, err := config.LoadStorageConfig()
	if err != nil {
		t.Fatal(err)
	}
	secure := settings.EndpointURL.Scheme == "https"
	transport, err := minio.DefaultTransport(secure)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.CloseIdleConnections)
	client, err := minio.New(settings.EndpointURL.Host, &minio.Options{
		Creds: credentials.NewStaticV4(settings.AccessKey, settings.SecretKey, ""), Secure: secure,
		Transport: transport, BucketLookup: minio.BucketLookupPath, MaxRetries: 1,
	})
	if err != nil {
		t.Fatal("创建联测 MinIO 客户端失败")
	}
	const bucketPrefix = "cloudnest-upload-it-"
	bucket := bucketPrefix + uuid.NewString()
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatal("创建独占上传联测桶失败", minio.ToErrorResponse(err).Code)
	}
	// 只在成功创建本次随机桶之后登记清理。
	t.Cleanup(func() {
		if _, err := uuid.Parse(strings.TrimPrefix(bucket, bucketPrefix)); !strings.HasPrefix(bucket, bucketPrefix) || err != nil {
			t.Error("拒绝清理不属于本次联测的桶")
			return
		}
		cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for object := range client.ListObjects(cleanupContext, bucket, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
			if object.Err != nil {
				t.Error("列举独占桶对象版本失败")
				return
			}
			if err := client.RemoveObject(cleanupContext, bucket, object.Key, minio.RemoveObjectOptions{VersionID: object.VersionID}); err != nil {
				t.Error("清理独占桶对象版本失败")
			}
		}
		if err := client.RemoveBucket(cleanupContext, bucket); err != nil {
			t.Error("删除独占上传联测桶失败")
			return
		}
		if exists, err := client.BucketExists(cleanupContext, bucket); err != nil || exists {
			t.Error("独占桶清理复核失败")
		}
	})
	if err := client.EnableVersioning(ctx, bucket); err != nil {
		t.Fatal("开启独占桶版本控制失败", err)
	}
	t.Setenv("CLOUDNEST_S3_BUCKET", bucket)
	return &serverUploadContentBucket{client: client, name: bucket}
}

func verifyServerUploadContent(t *testing.T, ctx context.Context, pool *sql.DB, target *serverUploadContentBucket, actor domain.Actor, baseURL, token, otherToken string) {
	t.Helper()
	store, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Timeout: 5 * time.Second}
	defer httpClient.CloseIdleConnections()
	put := func(t *testing.T, created dto.CreateUploadData, actorToken string, payload []byte, status int, code string) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodPut, baseURL+created.ContentPath, bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		requestID := uuid.NewString()
		request.Header.Set("X-Request-ID", requestID)
		request.Header.Set("Authorization", "Bearer "+actorToken)
		request.Header.Set("Content-Type", "application/octet-stream")
		response, err := httpClient.Do(request)
		if err != nil {
			t.Fatal("正式上传请求失败", err)
		}
		body, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if response.StatusCode != status || readError != nil || closeError != nil ||
			response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("正式上传响应状态或头错误", response.StatusCode, string(body), readError, closeError)
		}
		if status == http.StatusAccepted {
			var result dto.UploadContentAcceptedResponse
			if json.Unmarshal(body, &result) != nil || result.Data.TaskID != created.Task.ID ||
				result.Data.Status != "VERIFYING" || result.RequestID != requestID {
				t.Fatal("正式上传接收响应错误", string(body))
			}
			return
		}
		var result dto.ErrorResponse
		if json.Unmarshal(body, &result) != nil || result.Error.Code != code || result.RequestID != requestID {
			t.Fatal("正式上传失败交付错误", string(body))
		}
	}

	before := readServerUploadCreationState(t, ctx, pool)
	userID := actor.UserID
	var usedBefore uint64
	if err := pool.QueryRowContext(ctx, "SELECT used_storage FROM users WHERE id=?", userID).Scan(&usedBefore); err != nil {
		t.Fatal(err)
	}
	binary := []byte{0, 0xff, 0xfe, 1, 0x80}
	var received []*domain.UploadTask
	for _, fixture := range []struct {
		name, key string
		data      []byte
		wrongHash bool
	}{
		{"binary", "server-upload-receive-binary", binary, false},
		{"empty", "server-upload-receive-empty", nil, false},
		{"checksum mismatch", "server-upload-receive-checksum", binary, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 通过正式创建接口取得已有任务和对象位置。
			expected := sha256.Sum256(fixture.data)
			if fixture.wrongHash {
				expected = sha256.Sum256([]byte("different bytes"))
			}
			input := fmt.Sprintf(`{"parent_id":"33","name":"receive.bin","size_bytes":%d,"sha256":"%x"}`, len(fixture.data), expected)
			created := requestServerCreateUpload(t, baseURL, token, fixture.key, input, 201, "", "").Data
			taskID, fieldError := dto.ParseUploadTaskPath(created.Task.ID)
			if fieldError != nil {
				t.Fatal("创建结果没有合法任务 ID")
			}
			read := func() (*domain.UploadTask, *domain.StoredObject) {
				t.Helper()
				task, err := store.FindUserUploadTask(ctx, userID, taskID.TaskID)
				if err != nil {
					t.Fatal(err)
				}
				object, err := store.FindUserStoredObject(ctx, userID, task.StagedObjectID)
				if err != nil || object.Bucket != target.name {
					t.Fatal("任务未绑定本次独占存储桶", err)
				}
				return task, object
			}
			pending, original := read()
			if fixture.name == "binary" {
				put(t, created, otherToken, fixture.data, 404, "UPLOAD_NOT_FOUND")
				put(t, created, token, fixture.data[:len(fixture.data)-1], 413, "BODY_SIZE_MISMATCH")
				task, object := read()
				if task.LeaseVersion != 0 || object.ID != original.ID || task.Status != domain.UploadStatusUploading {
					t.Fatal("前置拒绝改变了任务准备状态")
				}
			}

			// 2. 经正式 PUT 检查正文，核对状态推进和物理写入。
			if fixture.wrongHash {
				put(t, created, token, fixture.data, 422, "CHECKSUM_MISMATCH")
				task, object := read()
				_, statError := target.client.StatObject(ctx, target.name, object.ObjectKey, minio.StatObjectOptions{})
				if task.Status != domain.UploadStatusUploading || object.Status != domain.StoredObjectStatusStaging ||
					object.ID != original.ID || minio.ToErrorResponse(statError).Code != "NoSuchKey" {
					t.Fatal("摘要错误改变了状态或写入了对象")
				}
				return
			}
			put(t, created, token, fixture.data, 202, "")
			put(t, created, token, fixture.data, 409, "TASK_NOT_READY")
			_, object := read()
			if object.ID != original.ID || object.StorageETag == nil || object.StorageVersionID == nil ||
				(object.Status != domain.StoredObjectStatusVerifying && object.Status != domain.StoredObjectStatusReady) {
				t.Fatal("写入确认、对象身份或状态错误")
			}
			source, err := target.client.GetObject(ctx, target.name, object.ObjectKey, minio.GetObjectOptions{VersionID: *object.StorageVersionID})
			if err != nil {
				t.Fatal("打开联测对象失败")
			}
			defer source.Close()
			info, statError := source.Stat()
			actual, readError := io.ReadAll(io.LimitReader(source, int64(len(fixture.data))+1))
			if statError != nil || readError != nil || info.Size != int64(len(fixture.data)) ||
				info.ETag != *object.StorageETag || !bytes.Equal(actual, fixture.data) {
				t.Fatal("MinIO 读回内容或确认信息不符", statError, readError)
			}
			received = append(received, pending)
		})
	}

	t.Run("background verification", func(t *testing.T) {
		// 1. 只等待生产 worker，校验轮次由服务调度。
		if len(received) != 2 {
			t.Fatal("未准备好两条待校验记录", len(received))
		}
		waitContext, stopWaiting := context.WithTimeout(ctx, 12*time.Second)
		defer stopWaiting()

		// 2. 从查询接口和数据库核对已发布结果、实测摘要及配额结算。
		settings, err := config.LoadStorageConfig()
		if err != nil {
			t.Fatal(err)
		}
		settings.Bucket, settings.PublicEndpointURL = target.name, settings.EndpointURL
		downloadStorage, err := storage.NewClient(ctx, settings)
		if err != nil {
			t.Fatal("创建联测下载客户端失败", err)
		}
		defer downloadStorage.Close()
		downloadTargets := service.NewEntryDownloadTargetService(func(requestContext context.Context, read func(context.Context, service.EntryDownloadTargetReader) error) error {
			return store.WithFileEntrySnapshot(requestContext, func(snapshotContext context.Context, snapshot *repository.Store) error {
				return read(snapshotContext, snapshot)
			})
		})
		downloads := service.NewEntryDownloadService(downloadTargets.Get, downloadStorage.CheckDownloadObject, downloadStorage.SignDownloadURL,
			func(requestContext context.Context, owner domain.Actor, action func(context.Context, service.DownloadAccessTransaction) error) error {
				return store.WithUserTransaction(requestContext, owner, func(txContext context.Context, txStore *repository.Store) error {
					return action(txContext, txStore)
				})
			})
		var publishedBytes uint64
		for _, original := range received {
			task := awaitServerUploadPublication(t, waitContext, store, userID, original.ID)
			if task.ResultEntryID == nil ||
				task.PublishedObjectID == nil || *task.PublishedObjectID != original.StagedObjectID ||
				task.LeaseVersion != original.LeaseVersion+2 || task.LeaseOwner != nil || task.ReservedBytes != 0 {
				t.Fatal("校验任务完成记录不符", task)
			}
			object, err := store.FindUserStoredObject(ctx, userID, original.StagedObjectID)
			if err != nil || object.Status != domain.StoredObjectStatusReady || object.VerifiedSHA256 == nil || original.ExpectedSHA256 == nil ||
				*object.VerifiedSHA256 != *original.ExpectedSHA256 {
				t.Fatal("真实读回摘要未按上传声明写入", object, err)
			}
			queried := requestServerUploadTask(t, baseURL, token, strconv.FormatUint(task.ID, 10), 200, "", "")
			if queried.Data.Status != "COMPLETED" || queried.Data.ResultEntryID == nil || *queried.Data.ResultEntryID != strconv.FormatUint(*task.ResultEntryID, 10) {
				t.Fatal("正式查询未返回完成状态和目录项", queried.Data)
			}
			confirmed := requestServerUploadCompletion(t, ctx, baseURL, token, queried.Data.ID, 200, "", "")
			if confirmed.Data.Status != "COMPLETED" || confirmed.Data.ResultEntryID == nil ||
				*confirmed.Data.ResultEntryID != *queried.Data.ResultEntryID || confirmed.Data.CompletedAt == nil ||
				task.CompletedAt == nil || !confirmed.Data.CompletedAt.Equal(*task.CompletedAt) {
				t.Fatal("正式完成确认未返回后台实际发布结果", confirmed.Data)
			}
			verifyServerPublishedDownload(t, ctx, pool, httpClient, downloads, actor, task)
			publishedBytes += original.ReservedBytes
		}
		var usedAfter uint64
		if err := pool.QueryRowContext(ctx, "SELECT used_storage FROM users WHERE id=?", userID).Scan(&usedAfter); err != nil {
			t.Fatal(err)
		}
		settled := readServerUploadCreationState(t, ctx, pool)
		if usedAfter != usedBefore+publishedBytes || settled.reserved != before.reserved+uint64(len(binary)) || settled.taskReserved != settled.reserved ||
			settled.tasks != before.tasks+3 || settled.objects != before.objects+3 {
			t.Fatal("从预占转为已用的配额不符", usedBefore, usedAfter, settled)
		}
	})
}

// verifyServerPublishedDownload 用实际发布结果验证内部下载授权和真实 MinIO GET。
func verifyServerPublishedDownload(t *testing.T, ctx context.Context, pool *sql.DB, httpClient *http.Client, downloads *service.EntryDownloadService, actor domain.Actor, task *domain.UploadTask) {
	t.Helper()
	// 1. 尚未保存 MIME 时，拒绝预览且不写访问时间。
	entryID := *task.ResultEntryID
	readOpenedAt := func() time.Time {
		var openedAt sql.NullTime
		err := pool.QueryRowContext(ctx, "SELECT last_opened_at FROM user_entry_preferences WHERE user_id=? AND entry_id=?", actor.UserID, entryID).Scan(&openedAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("读取真实最近访问记录失败", err)
		}
		return openedAt.Time
	}
	if blocked, err := downloads.Create(ctx, actor, entryID, 1, domain.DownloadPurposePreview); blocked != nil ||
		!errors.Is(err, domain.ErrUnsupportedPreview) || !readOpenedAt().IsZero() {
		t.Fatal("未授权的预览处理或访问记录不符", err)
	}

	// 2. 确认下载授权写入真实时间，并验证六十秒合并规则。
	result, err := downloads.Create(ctx, actor, entryID, 1, domain.DownloadPurposeDownload)
	if err != nil || result == nil || result.ObjectID != *task.PublishedObjectID || result.Revision != 1 ||
		result.SizeBytes != task.FileSize || result.VerifiedSHA256 != *task.ExpectedSHA256 ||
		result.MIMEType != "application/octet-stream" || !result.Link.ExpiresAt.After(time.Now()) {
		t.Fatal("真实发布文件的内部下载授权不符", err)
	}
	firstOpenedAt := readOpenedAt()
	if firstOpenedAt.IsZero() {
		t.Fatal("首次签发未提交最近访问时间")
	}
	if repeated, err := downloads.Create(ctx, actor, entryID, 1, domain.DownloadPurposeDownload); err != nil || repeated == nil ||
		!readOpenedAt().Equal(firstOpenedAt) {
		t.Fatal("窗口内授权未复用原访问时间", err)
	}
	const setPastAccess = "UPDATE user_entry_preferences SET last_opened_at=UTC_TIMESTAMP(6)-INTERVAL 61 SECOND WHERE user_id=? AND entry_id=?"
	if _, err := pool.ExecContext(ctx, setPastAccess, actor.UserID, entryID); err != nil {
		t.Fatal("设置联测访问时间失败", err)
	}
	pastOpenedAt := readOpenedAt()
	if renewed, err := downloads.Create(ctx, actor, entryID, 1, domain.DownloadPurposeDownload); err != nil || renewed == nil ||
		!readOpenedAt().After(pastOpenedAt) {
		t.Fatal("窗口外授权未更新访问时间", err)
	}

	// 3. 将授权编码为公开 JSON，从解码后的链接读取实际文件。
	publicData, err := dto.NewCreateDownloadData(result)
	if err != nil {
		t.Fatal("真实授权无法转换为公开下载数据", err)
	}
	requestID := uuid.NewString()
	wire, err := json.Marshal(dto.CreateDownloadResponse{Data: *publicData, RequestID: requestID})
	if err != nil {
		t.Fatal("真实下载响应编码失败", err)
	}
	var decoded dto.CreateDownloadResponse
	if err := json.Unmarshal(wire, &decoded); err != nil || decoded.RequestID != requestID ||
		decoded.Data.ObjectID != strconv.FormatUint(*task.PublishedObjectID, 10) || decoded.Data.Revision != "1" ||
		!decoded.Data.ExpiresAt.Equal(result.Link.ExpiresAt) || decoded.Data.URL != result.Link.URL {
		t.Fatal("下载 JSON 未保留公开标识、签名地址和到期时间")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, decoded.Data.URL, nil)
	if err != nil {
		t.Fatal("无法构造联测签名请求")
	}
	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatal("实际发布文件的签名链接访问失败")
	}
	data, readError := io.ReadAll(io.LimitReader(response.Body, int64(task.FileSize)+1))
	closeError := response.Body.Close()
	if readError != nil || closeError != nil || response.StatusCode != http.StatusOK || uint64(len(data)) != decoded.Data.SizeBytes ||
		fmt.Sprintf("%x", sha256.Sum256(data)) != decoded.Data.SHA256 || response.Header.Get("Content-Type") != decoded.Data.MIMEType ||
		response.Header.Get("Content-Disposition") != decoded.Data.Disposition {
		t.Fatal("实际发布文件的远程字节或交付头不符", response.StatusCode)
	}
}

// awaitServerUploadPublication 轮询已交给生产 worker 的任务，完成后交还数据库事实。
func awaitServerUploadPublication(t *testing.T, ctx context.Context, store *repository.Store, userID, taskID uint64) *domain.UploadTask {
	t.Helper()
	// 1. 定期读取实际状态，完成或进入意外状态时结束等待。
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		current, err := store.FindUserUploadTask(ctx, userID, taskID)
		if err != nil {
			t.Fatal("等待后台发布时读取任务失败", taskID, err)
		}
		if current.Status == domain.UploadStatusCompleted {
			return current
		}
		if current.Status != domain.UploadStatusVerifying {
			t.Fatal("后台处理进入了意外的任务状态", current)
		}
		// 2. 到时观察下一次状态，等待期限由调用方控制。
		select {
		case <-ctx.Done():
			t.Fatal("后台校验未在等待期限内发布", current, ctx.Err())
		case <-poll.C:
		}
	}
}
