package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/storage"
)

func TestServerMultipartAbortObservationIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_ABORT_OBSERVATION_TEST") != "1" {
		t.Skip("显式启用后创建本轮专用 MinIO 容器和版本桶")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	containerID := startIsolatedWebMinIO(t, ctx)
	t.Logf("专用 MinIO %s，回环地址 %s", containerID[:12], os.Getenv("CLOUDNEST_S3_ENDPOINT"))
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
	verifyInstance := isolatedMinIOVerifier(containerID)
	core := minio.Core{Client: bucket.client}
	httpClient := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(httpClient.CloseIdleConnections)
	createSession := func(key string) multipartProbe {
		t.Helper()
		uploadID, err := client.CreateMultipartUpload(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanupContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := core.AbortMultipartUpload(cleanupContext, bucket.name, key, uploadID); err != nil && minio.ToErrorResponse(err).Code != "NoSuchUpload" {
				t.Error("专用会话清理失败", minio.ToErrorResponse(err).Code)
			}
		})
		return multipartProbe{core: core, bucket: bucket.name, key: key, uploadID: uploadID, endpoint: settings.EndpointURL}
	}

	// 1. 暂停一个已进入 MinIO 的分片 PUT。原会话仍存在，读到空页也得继续等待。
	inFlight := createSession("b4/abort-in-flight.bin")
	releasePart, partResult := startPausedMultipartPart(t, ctx, httpClient, inFlight)
	waitForMinIOPartTempFile(t, ctx, containerID)
	page, err := client.ListUploadPartsPage(ctx, inFlight.key, inFlight.uploadID, 0, 1)
	if err != nil || page == nil || len(page.Parts) != 0 {
		t.Fatal("在途分片应尚未提交到原会话", err)
	}
	pending, err := client.CheckMultipartUploadAfterAbort(ctx, inFlight.key, inFlight.uploadID, verifyInstance)
	if pending != nil || err != nil {
		t.Fatal("在途分片尚未提交时仍应等待原会话", pending, err)
	}

	// 2. 中止原会话并取得缺失观察，再放行剩余字节，确认迟到的 PUT 无法留下分片。
	if err := client.AbortMultipartUpload(ctx, inFlight.key, inFlight.uploadID); err != nil {
		t.Fatal("无法中止本次合成会话", err)
	}
	absent, err := client.CheckMultipartUploadAfterAbort(ctx, inFlight.key, inFlight.uploadID, verifyInstance)
	if absent == nil || absent.Object != nil || err != nil {
		t.Fatal("中止后应确认会话与对象均缺失", absent, err)
	}
	if err := releasePart(); err != nil {
		t.Fatal("未能放行在途分片的剩余字节", err)
	}
	select {
	case response := <-partResult:
		if response.status != http.StatusNotFound || response.code != "NoSuchUpload" {
			t.Fatal("在途分片收尾未得到明确拒绝", response.status, response.code)
		}
	case <-ctx.Done():
		t.Fatal("等待在途分片结果超时")
	}
	absent, err = client.CheckMultipartUploadAfterAbort(ctx, inFlight.key, inFlight.uploadID, verifyInstance)
	if absent == nil || absent.Object != nil || err != nil {
		t.Fatal("在途分片结束后应仍能确认会话和对象均缺失", absent, err)
	}
	if observation, err := client.CheckMultipartUploadAfterAbort(ctx, inFlight.key, inFlight.uploadID, nil); observation != nil || err != nil {
		t.Fatal("缺少受信实例时应继续等待", observation, err)
	}
	t.Log("MinIO 已写入在途分片的临时文件。中止前 ListParts 为空但会话仍存在，中止后迟到的 PUT 返回 404 NoSuchUpload，原会话和对象仍缺失")

	// 3. 合成会话先生成对象，后续中止和复查应保留确切的对象版本。
	completed := createSession("b4/abort-after-complete.bin")
	payload := []byte("synthetic completed multipart")
	checksum := multipartProbeSHA256(payload)
	signed := completed.sign(t, ctx, 1, len(payload), checksum)
	etag := putMultipartProbePart(t, ctx, httpClient, signed, checksum, payload, http.StatusOK, "")
	version, err := core.CompleteMultipartUpload(ctx, bucket.name, completed.key, completed.uploadID,
		[]minio.CompletePart{{PartNumber: 1, ETag: etag, ChecksumSHA256: checksum}}, minio.PutObjectOptions{})
	if err != nil || version.VersionID == "" || version.VersionID == "null" {
		t.Fatal("合成分片未生成版本化对象", minio.ToErrorResponse(err).Code)
	}
	if err := client.AbortMultipartUpload(ctx, completed.key, completed.uploadID); err != nil {
		t.Fatal("固定 MinIO 应确认已结束会话的中止请求", err)
	}
	observed, err := client.CheckMultipartUploadAfterAbort(ctx, completed.key, completed.uploadID, verifyInstance)
	if err != nil || observed == nil || observed.Object == nil {
		t.Fatal("复查未找到已生成对象", err)
	}
	object := observed.Object
	if object.StorageVersionID == nil || *object.StorageVersionID != version.VersionID ||
		object.StorageETag == nil || *object.StorageETag != version.ETag || object.SizeBytes != uint64(len(payload)) {
		t.Fatal("复查的版本、ETag 或大小与原合并结果不一致")
	}
	t.Logf("先完成的会话在中止后仍复查到原对象版本，大小 %d B，版本和 ETag 一致", object.SizeBytes)
}

type pausedMultipartPartResult struct {
	status int
	code   string
}

type pausedMultipartPartBody struct {
	io.Reader
	io.Closer
}

func startPausedMultipartPart(
	t *testing.T, ctx context.Context, httpClient *http.Client, probe multipartProbe,
) (release func() error, result <-chan pausedMultipartPartResult) {
	t.Helper()
	// 1. 发送数个 MinIO 编码块，只暂停最后一字节。签名摘要覆盖完整合成分片。
	payload := bytes.Repeat([]byte{0x51}, (8<<20)+1)
	digest := multipartProbeSHA256(payload)
	signed := probe.sign(t, ctx, 1, len(payload), digest)
	tail, writer := io.Pipe()
	t.Cleanup(func() {
		_ = writer.Close()
		_ = tail.Close()
	})
	body := pausedMultipartPartBody{Reader: io.MultiReader(bytes.NewReader(payload[:len(payload)-1]), tail), Closer: tail}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, signed.String(), body)
	if err != nil {
		t.Fatal("无法构造在途分片请求")
	}
	request.ContentLength = int64(len(payload))
	request.Header.Set(multipartProbeChecksumHeader, digest)

	// 2. 在独立协程读回响应，主测试用临时文件确认存储已经开始写分片。
	responses := make(chan pausedMultipartPartResult, 1)
	go func() {
		response, err := httpClient.Do(request)
		if err != nil {
			responses <- pausedMultipartPartResult{}
			return
		}
		defer response.Body.Close()
		var body struct{ Code string }
		_ = xml.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&body)
		responses <- pausedMultipartPartResult{status: response.StatusCode, code: body.Code}
	}()
	return func() error {
		defer writer.Close()
		_, err := writer.Write(payload[len(payload)-1:])
		return err
	}, responses
}

// waitForMinIOPartTempFile 只读核对本次独占容器的暂存目录。
// 暂存文件在 MinIO 已找到会话并开始编码分片后出现，排除请求只在客户端排队的情况。
func waitForMinIOPartTempFile(t *testing.T, ctx context.Context, containerID string) {
	t.Helper()
	check, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		output, err := exec.CommandContext(check, "docker", "exec", containerID, "find", "/data/.minio.sys/tmp", "-type", "f", "-name", "part.1").Output()
		if err != nil {
			t.Fatal("无法读取专用 MinIO 的分片暂存目录", err)
		}
		if strings.TrimSpace(string(output)) != "" {
			return
		}
		select {
		case <-check.Done():
			t.Fatal("在途分片未在期限内生成临时文件")
		case <-tick.C:
		}
	}
}
