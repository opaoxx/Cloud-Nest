package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
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
	t.Setenv("CLOUDNEST_S3_BUCKET", bucket)
	return &serverUploadContentBucket{client: client, name: bucket}
}

func verifyServerUploadContent(t *testing.T, ctx context.Context, pool *sql.DB, target *serverUploadContentBucket, baseURL, token, otherToken string) {
	t.Helper()
	if err := target.client.EnableVersioning(ctx, target.name); err != nil {
		t.Fatal("开启独占桶版本控制失败", err)
	}
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
				task, err := store.FindUserUploadTask(ctx, 3, taskID.TaskID)
				if err != nil {
					t.Fatal(err)
				}
				object, err := store.FindUserStoredObject(ctx, 3, task.StagedObjectID)
				if err != nil || object.Bucket != target.name {
					t.Fatal("任务未绑定本次独占存储桶", err)
				}
				return task, object
			}
			_, original := read()
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
			task, object := read()
			if task.Status != domain.UploadStatusVerifying || task.LeaseOwner != nil || task.LeaseUntil != nil ||
				task.LeaseVersion != 1 || task.ReservedBytes != uint64(len(fixture.data)) || object.ID != original.ID ||
				object.Status != domain.StoredObjectStatusVerifying || object.StorageETag == nil || object.StorageVersionID == nil || object.VerifiedAt != nil {
				t.Fatal("写入确认、对象身份、两表状态或租约记录错误")
			}
			source, err := target.client.GetObject(ctx, target.name, object.ObjectKey, minio.GetObjectOptions{})
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
			queried := requestServerUploadTask(t, baseURL, token, created.Task.ID, 200, "", "")
			if queried.Data.Status != "VERIFYING" || queried.Data.ResultEntryID != nil {
				t.Fatal("任务查询结果与实际校验阶段不符")
			}
			received = append(received, task)
			if fixture.name == "binary" {
				// 3. 为同一键写入不同的最新版本，验证校验会按任务记录读取原版本。
				const laterContent = "later version"
				latest, err := target.client.PutObject(ctx, target.name, object.ObjectKey, strings.NewReader(laterContent), int64(len(laterContent)), minio.PutObjectOptions{})
				if err != nil || latest.VersionID == "" || latest.VersionID == *object.StorageVersionID {
					t.Fatal("写入独占对象的新版本失败", err)
				}
			}
		})
	}
	after := readServerUploadCreationState(t, ctx, pool)
	if after.reserved != before.reserved+2*uint64(len(binary)) || after.taskReserved != after.reserved ||
		after.tasks != before.tasks+3 || after.objects != before.objects+3 {
		t.Fatal("上传接收改变了预占或重复创建了任务和对象")
	}

	t.Run("verification round", func(t *testing.T) {
		// 1. 复用启动配置连接独占桶，绑定生产单轮校验入口。
		settings, err := config.LoadStorageConfig()
		if err != nil {
			t.Fatal(err)
		}
		client, err := storage.NewClient(ctx, settings)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if len(received) != 2 {
			t.Fatal("未准备好两条待校验记录", len(received))
		}
		const userID = 3
		var usedBefore uint64
		if err := pool.QueryRowContext(ctx, "SELECT used_storage FROM users WHERE id=?", userID).Scan(&usedBefore); err != nil {
			t.Fatal(err)
		}
		batch := newServerUploadVerificationBatch(store, client)
		summary, err := batch.RunOnce(ctx, 2, "server-verification-test")
		if err != nil || summary.Scanned != 2 || summary.Published != 2 || summary.RetriesScheduled != 0 || len(summary.CandidateErrors) != 0 {
			t.Fatal("生产校验入口未发布两条真实上传", summary, err)
		}

		// 2. 从查询接口和数据库核对已发布结果、实测摘要及配额结算。
		var publishedBytes uint64
		for _, original := range received {
			task, err := store.FindUserUploadTask(ctx, userID, original.ID)
			if err != nil || task.Status != domain.UploadStatusCompleted || task.ResultEntryID == nil ||
				task.PublishedObjectID == nil || *task.PublishedObjectID != original.StagedObjectID ||
				task.LeaseVersion != original.LeaseVersion+1 || task.LeaseOwner != nil || task.ReservedBytes != 0 {
				t.Fatal("校验任务完成记录不符", task, err)
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
			publishedBytes += original.ReservedBytes
		}
		var usedAfter uint64
		if err := pool.QueryRowContext(ctx, "SELECT used_storage FROM users WHERE id=?", userID).Scan(&usedAfter); err != nil {
			t.Fatal(err)
		}
		settled := readServerUploadCreationState(t, ctx, pool)
		if usedAfter != usedBefore+publishedBytes || settled.reserved != after.reserved-publishedBytes || settled.taskReserved != settled.reserved ||
			settled.tasks != after.tasks || settled.objects != after.objects {
			t.Fatal("从预占转为已用的配额不符", usedBefore, usedAfter, settled)
		}
		if repeated, err := batch.RunOnce(ctx, 2, "server-verification-test"); err != nil || repeated.Scanned != 0 || len(repeated.CandidateErrors) != 0 {
			t.Fatal("完成任务仍被再次扫描", repeated, err)
		}
	})
}
