package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

func TestServerMultipartInterruptedPartIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_INTERRUPTED_PART_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_INTERRUPTED_PART_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("分片中断专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])

	// 1. 通过正式入口创建两片任务，完整上传首片并取得回执。
	const firstSize = domain.MaxSingleUploadSizeBytes
	const tailSize = 64 << 10
	const fileSize = firstSize + tailSize
	payload := make([]byte, fileSize)
	payload[0], payload[firstSize], payload[len(payload)-1] = 0x31, 0x32, 0x33
	body := fmt.Sprintf(`{"parent_id":%q,"name":"multipart-interrupted-fixture.bin","size_bytes":%d,"sha256":"%x"}`,
		fixture.login.RootEntryID, fileSize, sha256.Sum256(payload))
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	api := fixture.api
	api.request(t, ctx, http.MethodPost, "/api/v1/uploads", "multipart-interrupted-fixture", strings.NewReader(body), http.StatusCreated, &created)
	if created.RequestID == "" || created.Data.Task.Status != "CREATING" || created.Data.UploadMode != "MULTIPART" ||
		created.Data.PartSizeBytes != firstSize || created.Data.PartCount != 2 {
		t.Fatal("合成任务没有取得预期的两片计划", created.Data)
	}
	taskPath := "/api/v1/uploads/" + created.Data.Task.ID
	readyTask := waitForCreateUploadInitialization(t, ctx, api, taskPath)
	target := readServerMultipartPartTarget(t, ctx, fixture, readyTask)
	firstPart := payload[:firstSize]
	firstURL, firstChecksum := target.signPart(t, ctx, api, 1, firstPart)
	firstETag := putMultipartProbePart(t, ctx, api.httpClient, firstURL, firstChecksum, firstPart, http.StatusOK, "")
	expected := dto.UploadPartReceiptData{PartNumber: 1, SizeBytes: firstSize, ETag: firstETag, ChecksumSHA256: firstChecksum}
	readParts := func() dto.UploadPartsData {
		t.Helper()
		var response dto.UploadPartsResponse
		api.request(t, ctx, http.MethodGet, taskPath+"/parts", "", nil, http.StatusOK, &response)
		if response.RequestID == "" || response.Data.PartSizeBytes != firstSize || response.Data.PartCount != 2 {
			t.Fatal("读取分片计划失败", response.Data)
		}
		return response.Data
	}
	before := readParts()
	if !slices.Equal(before.Parts, []dto.UploadPartReceiptData{expected}) {
		t.Fatal("中断前的首片回执不符", before.Parts)
	}

	// 2. 签发绑定完整长度的尾片 PUT，只发送一半正文后关闭发送方向。
	tail := payload[firstSize:]
	tailURL, tailChecksum := target.signPart(t, ctx, api, 2, tail)
	sendInterruptedMultipartPart(t, ctx, tailURL, tailChecksum, tail)

	// 3. 核对服务仍可读到原首片，未完成的尾片没有回执，配额保持预占。
	after := readParts()
	if !reflect.DeepEqual(after, before) {
		t.Fatal("连接中断改变了已确认的回执", before, after)
	}
	var current dto.UploadTaskResponse
	api.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &current)
	var profile dto.UserProfileResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	if !reflect.DeepEqual(current.Data, readyTask) || profile.Data.User.ID != fixture.login.User.ID ||
		profile.Data.UsedBytes != 0 || profile.Data.ReservedBytes != fileSize {
		t.Fatal("连接中断改变了任务状态或配额")
	}
	var sessionID string
	var completedParts, publishedFiles uint64
	if err := fixture.pool.QueryRowContext(ctx, `SELECT storage_upload_id,
		(SELECT COUNT(*) FROM upload_parts WHERE task_id=task.id),
		(SELECT COUNT(*) FROM file_entries WHERE user_id=task.user_id AND entry_type='FILE')
		FROM upload_tasks task WHERE id=? AND user_id=?`, readyTask.ID, fixture.login.User.ID).
		Scan(&sessionID, &completedParts, &publishedFiles); err != nil ||
		sessionID != target.uploadID || completedParts != 0 || publishedFiles != 0 {
		t.Fatal("连接中断改变了绑定会话、完成快照或发布文件", err, completedParts, publishedFiles)
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, api.baseURL, logs)
	t.Logf("原任务 %s 中断后仅保留首片 %d B，ETag %s，SHA-256 %s，尾片没有回执，已用 %d B、预占 %d B",
		readyTask.ID, expected.SizeBytes, expected.ETag, expected.ChecksumSHA256, profile.Data.UsedBytes, profile.Data.ReservedBytes)
}

func sendInterruptedMultipartPart(t *testing.T, ctx context.Context, signed *url.URL, checksum string, payload []byte) {
	t.Helper()
	// 1. 在独占 MinIO 的回环地址建立一条 TCP 连接，发送完整声明和部分正文。
	if signed.Scheme != "http" || signed.Hostname() != "127.0.0.1" || len(payload) < 2 {
		t.Fatal("尾片中断测试需要本轮隔离 HTTP 地址及可截断正文")
	}
	header := fmt.Sprintf("PUT %s HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n%s: %s\r\nConnection: close\r\n\r\n",
		signed.RequestURI(), signed.Host, len(payload), multipartProbeChecksumHeader, checksum)
	dialer := net.Dialer{Timeout: 5 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", signed.Host)
	if err != nil {
		t.Fatal("连接隔离 MinIO 失败")
	}
	defer connection.Close()
	socket := connection.(*net.TCPConn)
	if err := socket.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal("设置隔离连接期限失败", err)
	}
	if _, err := io.WriteString(socket, header); err != nil {
		t.Fatal("写入尾片请求头失败")
	}
	partial := payload[:len(payload)/2]
	sent, err := socket.Write(partial)
	if err != nil || sent != len(partial) {
		t.Fatal("未能写入预期的部分正文", sent)
	}

	// 2. 关闭发送方向让对端读到提前结束，保留接收方向以核实存储响应。
	if err := socket.CloseWrite(); err != nil {
		t.Fatal("无法中断尾片正文发送")
	}
	response, err := http.ReadResponse(bufio.NewReader(socket), &http.Request{Method: http.MethodPut})
	if err != nil {
		t.Fatal("读取尾片中断后的 MinIO 响应失败", err)
	}
	defer response.Body.Close()
	var failure struct{ Code string }
	if err := xml.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&failure); err != nil {
		t.Fatal("MinIO 中断响应缺少可解析的错误码", response.StatusCode, err)
	}
	// MinIO 可能先观察到请求被取消，也可能先读到少于声明的正文。
	clientDisconnected := response.StatusCode == 499 && failure.Code == "ClientDisconnected"
	incompleteBody := response.StatusCode == http.StatusBadRequest && failure.Code == "IncompleteBody"
	if (!clientDisconnected && !incompleteBody) || response.Header.Get("ETag") != "" {
		t.Fatalf("尾片部分正文未被预期的中断错误拒绝，HTTP %d，错误码 %s", response.StatusCode, failure.Code)
	}
	t.Logf("尾片声明 %d B，只发送 %d B 后关闭发送方向，MinIO 返回 %d %s",
		len(payload), sent, response.StatusCode, failure.Code)
}
