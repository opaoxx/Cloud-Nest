package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartCompletionAdapterIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_COMPLETE_ADAPTER_TEST") != "1" {
		t.Skip("显式启用后创建本轮专用 MinIO 容器和版本桶")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Second)
	defer cancel()
	containerID := startIsolatedWebMinIO(t, ctx)
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
	const key = "b4/synthetic +%.bin"
	const partSize = 16 << 20
	const fileSize = partSize + 1
	core := minio.Core{Client: bucket.client}
	uploadID, err := client.CreateMultipartUpload(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := core.AbortMultipartUpload(cleanupContext, bucket.name, key, uploadID); err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
			t.Error("专用会话清理失败", minio.ToErrorResponse(err).Code)
		}
	})
	probe := multipartProbe{core: core, bucket: bucket.name, key: key, uploadID: uploadID, endpoint: settings.EndpointURL}
	httpClient := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(httpClient.CloseIdleConnections)
	put := func(number int, payload []byte) string {
		t.Helper()
		digest := multipartProbeSHA256(payload)
		return putMultipartProbePart(t, ctx, httpClient, probe.sign(t, ctx, number, len(payload), digest), digest, payload, http.StatusOK, "")
	}

	// 1. 在独占会话中写入两片，再替换首片，保留旧回执用于验证拒绝逻辑。
	first := bytes.Repeat([]byte{0x11}, partSize)
	replacement := bytes.Repeat([]byte{0x22}, partSize)
	tail := []byte{0x33}
	oldETag := put(1, first)
	put(2, tail)
	newETag := put(1, replacement)
	receipts, err := client.ListUploadParts(ctx, key, uploadID, 1)
	if err != nil || len(receipts) != 2 || oldETag == newETag || receipts[0].ETag != newETag || receipts[0].SizeBytes != partSize || receipts[1].SizeBytes != 1 {
		t.Fatal("未取得隔离会话的正确分片回执", err)
	}
	stale := []domain.UploadPartReceipt{receipts[0], receipts[1]}
	stale[0].ETag, stale[0].ChecksumSHA256 = oldETag, multipartProbeSHA256(first)
	rejected, partError := client.CompleteMultipartUpload(ctx, key, uploadID, fileSize, partSize, 2, stale)
	if rejected != nil || !errors.Is(partError, domain.ErrMultipartUploadPartRejected) {
		t.Fatal("旧回执应得到明确拒绝", rejected, partError)
	}
	if result, err := client.StatMultipartUploadObject(ctx, key); result != nil || !errors.Is(err, domain.ErrUploadObjectMissing) {
		t.Fatal("拒绝后对象应仍缺失", result, err)
	}
	// 使用合成租约和冻结回执，通过真实 MinIO 只读核对原对象和原会话。
	lease := &domain.UploadTask{ID: 19, UserID: 7, StagedObjectID: 23, Status: domain.UploadStatusCompleting, UploadMode: "MULTIPART",
		StorageUploadID: &uploadID, LeaseVersion: 2, LeaseOwner: new("fixture-repair"), LeaseUntil: new(time.Now().UTC().Add(time.Minute)),
		PartSize: new(uint64(partSize)), PartCount: new(uint32(2)), FileSize: fileSize, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	target := service.UploadPartSigningTarget{TaskID: lease.ID, UserID: lease.UserID, StagedObjectID: lease.StagedObjectID,
		LeaseVersion: lease.LeaseVersion, ObjectKey: key, UploadID: uploadID, FileSizeBytes: fileSize, PartSizeBytes: partSize,
		PartCount: 2, ExpiresAt: lease.ExpiresAt}
	getTarget := func(context.Context, *domain.UploadTask) (*service.UploadPartSigningTarget, error) {
		return &target, nil
	}
	recovery := service.NewUploadMergeRecoveryService(getTarget, client.StatMultipartUploadObject)
	receiptCheck := service.NewUploadMergeReceiptService(getTarget,
		func(context.Context, *domain.UploadTask) ([]domain.UploadPartReceipt, error) { return stale, nil },
		func(callContext context.Context, key, session string) ([]domain.UploadPartReceipt, error) {
			return client.ListUploadParts(callContext, key, session, 1)
		})
	preflight := service.NewUploadMergePreflightService(recovery, receiptCheck)
	evidence, err := preflight.CheckRepair(ctx, lease, partError)
	if err != nil || evidence == nil || evidence.ObjectKey != key || evidence.Task.ID != lease.ID ||
		evidence.Task.LeaseVersion != lease.LeaseVersion || !slices.Equal(evidence.PartNumbers, []uint32{1}) {
		t.Fatal("原会话的首片变化应得到只读修复依据", evidence, err)
	}

	// 2. 用同一会话的当前回执执行一次合并，再按返回的精确版本校验内容。
	result, err := client.CompleteMultipartUpload(ctx, key, uploadID, fileSize, partSize, 2, receipts)
	if err != nil || result == nil || result.SizeBytes != fileSize || result.StorageETag == nil || result.StorageVersionID == nil || *result.StorageVersionID == "null" {
		t.Fatal("当前回执未得到有效的版本化合并确认", result, err)
	}
	version := *result.StorageVersionID
	if evidence, err := preflight.CheckRepair(ctx, lease, partError); evidence != nil || !errors.Is(err, domain.ErrUploadObjectExists) {
		t.Fatal("已完成对象不应交付修复依据", evidence, err)
	}
	reader, info, _, err := core.GetObject(ctx, bucket.name, key, minio.GetObjectOptions{VersionID: version})
	if err != nil {
		t.Fatal("未能读回确认的合成版本", minio.ToErrorResponse(err).Code)
	}
	defer reader.Close()
	expectedDigest := sha256.New()
	expectedDigest.Write(replacement)
	expectedDigest.Write(tail)
	actualDigest := sha256.New()
	readSize, err := io.Copy(actualDigest, io.LimitReader(reader, fileSize+1))
	if err != nil || readSize != fileSize || info.Size != fileSize || info.VersionID != version || info.ETag != *result.StorageETag ||
		!bytes.Equal(actualDigest.Sum(nil), expectedDigest.Sum(nil)) {
		t.Fatal("合并读回的大小、版本、ETag 或 SHA-256 不符", readSize, err)
	}

	// 3. 显式重发用于模拟重复派发，再用只读复核找回原对象版本。
	replay, replayError := client.CompleteMultipartUpload(ctx, key, uploadID, fileSize, partSize, 2, receipts)
	if replay != nil || !errors.Is(replayError, domain.ErrUploadPartSessionMissing) || !errors.Is(replayError, domain.ErrMultipartUploadCompletionUnknown) {
		t.Fatal("已关闭会话未返回恢复信号", replay, replayError)
	}
	checked, err := preflight.CheckMissingSession(ctx, lease, replayError)
	if err != nil || checked == nil || checked.Missing != nil || checked.Recovered == nil ||
		checked.Recovered.Target != target || checked.Recovered.Object.StorageVersionID == nil || *checked.Recovered.Object.StorageVersionID != version {
		t.Fatal("会话消失后未复核到原对象版本", checked, err)
	}
	versions := 0
	for stored := range bucket.client.ListObjects(ctx, bucket.name, minio.ListObjectsOptions{Prefix: key, Recursive: true, WithVersions: true}) {
		if stored.Err != nil || stored.Key != key || stored.VersionID != version || stored.IsDeleteMarker {
			t.Fatal("重复派发后读到非预期版本", minio.ToErrorResponse(stored.Err).Code)
		}
		versions++
	}
	if versions != 1 {
		t.Fatal("重复派发后版本数量不符", versions)
	}

	// 4. 在独占测试桶准备一个已中止的会话，验证复核只交付缺失观察。
	const missingKey = "b4/synthetic-absent.bin"
	missingUploadID, err := client.CreateMultipartUpload(ctx, missingKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := core.AbortMultipartUpload(ctx, bucket.name, missingKey, missingUploadID); err != nil {
		t.Fatal("无法中止合成会话", minio.ToErrorResponse(err).Code)
	}
	_, sessionError := client.ListUploadParts(ctx, missingKey, missingUploadID, 1)
	if !errors.Is(sessionError, domain.ErrUploadPartSessionMissing) {
		t.Fatal("夹具未取得会话缺失信号", sessionError)
	}
	missingLease := *lease
	missingLease.ID++
	missingLease.StagedObjectID++
	missingLease.StorageUploadID = &missingUploadID
	// 目标读取夹具改为返回第二个合成任务的绑定。
	target.TaskID, target.StagedObjectID, target.ObjectKey, target.UploadID = missingLease.ID, missingLease.StagedObjectID, missingKey, missingUploadID
	checked, err = preflight.CheckMissingSession(ctx, &missingLease, sessionError)
	if err != nil || checked == nil || checked.Recovered != nil || checked.Missing == nil ||
		checked.Missing.ObjectKey != missingKey || checked.Missing.Task.ID != missingLease.ID ||
		checked.Missing.Task.StorageUploadID == nil || *checked.Missing.Task.StorageUploadID != missingUploadID ||
		checked.Missing.Task.LeaseVersion != missingLease.LeaseVersion {
		t.Fatal("原会话和对象均缺失时未交付对应观察", checked, err)
	}
	t.Log("原会话消失后找回已完成版本，已中止的另一会话返回原对象及会话均缺失")
	t.Logf("专用 MinIO %s，回环地址 %s，合成版本 %s，读回 %d B，SHA-256 %x，版本数 %d",
		containerID[:12], settings.EndpointURL, version, readSize, actualDigest.Sum(nil), versions)
}
