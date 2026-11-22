package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

const multipartClientRestartArg = "cloudnest-isolated-restart-client"
const multipartClientSourceName = "restart-client-source.bin"

type multipartClientRestartRecord struct {
	Server        string                      `json:"server"`
	UserID        string                      `json:"user_id"`
	ParentID      string                      `json:"parent_id"`
	CreateKey     string                      `json:"create_key"`
	SourceName    string                      `json:"source_name"`
	SizeBytes     uint64                      `json:"size_bytes"`
	Digest        [sha256.Size]byte           `json:"sha256"`
	TaskID        string                      `json:"task_id"`
	PartSizeBytes uint64                      `json:"part_size_bytes"`
	PartCount     uint32                      `json:"part_count"`
	Receipts      []dto.UploadPartReceiptData `json:"receipts"`
}

func TestServerMultipartClientRestartIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MULTIPART_CLIENT_RESTART_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MULTIPART_CLIENT_RESTART_TEST=1 后启动独占 MySQL、MinIO 和服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	fixture := prepareBackupSource(t, ctx)
	t.Logf("客户端重启专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	directory := t.TempDir()
	const partSize uint64 = 16 << 20
	payload := make([]byte, partSize+1)
	payload[0], payload[partSize] = 0x31, 0x71
	if err := os.WriteFile(filepath.Join(directory, multipartClientSourceName), payload, 0600); err != nil {
		t.Fatal("无法保存隔离合成源", err)
	}

	// 1. 等待第一个客户端进程退出，用其留下的磁盘记录核对原会话。
	firstPID := runMultipartClientPhase(t, ctx, fixture, directory, "first-part")
	first := readMultipartClientRecord(t, directory)
	if first.UserID != fixture.login.User.ID || first.Server != fixture.api.baseURL || first.ParentID != fixture.login.RootEntryID ||
		first.SizeBytes != uint64(len(payload)) || first.Digest != sha256.Sum256(payload) ||
		first.PartSizeBytes != partSize || first.PartCount != 2 || len(first.Receipts) != 1 ||
		first.Receipts[0].PartNumber != 1 || first.Receipts[0].SizeBytes != partSize {
		t.Fatal("第一个客户端的持久任务或首片回执不符")
	}
	taskPath := "/api/v1/uploads/" + first.TaskID
	ready := waitForCreateUploadInitialization(t, ctx, fixture.api, taskPath)
	target := readServerMultipartPartTarget(t, ctx, fixture, ready)
	verifyMultipartClientReceipts(t, ctx, fixture.api, first)

	// 2. 启动第二个客户端。该进程从同一目录读取任务、源文件和首片回执。
	resumedPID := runMultipartClientPhase(t, ctx, fixture, directory, "resume")
	resumed := readMultipartClientRecord(t, directory)
	if resumedPID == firstPID || len(resumed.Receipts) != 2 ||
		resumed.Receipts[1].PartNumber != 2 || resumed.Receipts[1].SizeBytes != 1 {
		t.Fatal("新进程未持久化预期的尾片回执", firstPID, resumedPID)
	}
	expected := first
	expected.Receipts = append(slices.Clone(first.Receipts), resumed.Receipts[1])
	if !reflect.DeepEqual(resumed, expected) {
		t.Fatal("跨进程恢复改变了原任务计划、源身份或首片")
	}
	verifyMultipartClientReceipts(t, ctx, fixture.api, resumed)

	// 3. 独立核对仍是原任务、原会话及原预占，并检查没有完成写入。
	var current dto.UploadTaskResponse
	fixture.api.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &current)
	var profile dto.UserProfileResponse
	fixture.api.request(t, ctx, http.MethodGet, "/api/v1/me", "", nil, http.StatusOK, &profile)
	if current.RequestID == "" || !reflect.DeepEqual(current.Data, ready) || current.Data.Status != "UPLOADING" ||
		profile.RequestID == "" || profile.Data.User.ID != first.UserID || profile.Data.UsedBytes != 0 || profile.Data.ReservedBytes != first.SizeBytes {
		t.Fatal("续传后任务或配额不符", current.Data, profile.Data)
	}
	var session string
	var tasks, snapshots, completionRows uint64
	if err := fixture.pool.QueryRowContext(ctx, `SELECT storage_upload_id,
		(SELECT COUNT(*) FROM upload_tasks WHERE user_id=task.user_id),
		(SELECT COUNT(*) FROM idempotency_records WHERE user_id=task.user_id AND operation=?),
		(SELECT COUNT(*) FROM upload_parts WHERE task_id=task.id)
		FROM upload_tasks task WHERE id=? AND user_id=?`, domain.CreateUploadOperation, first.TaskID, first.UserID).
		Scan(&session, &tasks, &snapshots, &completionRows); err != nil ||
		session != target.uploadID || tasks != 1 || snapshots != 1 || completionRows != 0 {
		t.Fatal("续传后本轮会话、任务数或完成快照不符", err, tasks, snapshots, completionRows)
	}
	logs, err := fixture.stop()
	if err != nil {
		t.Fatal("隔离服务停机失败", err)
	}
	verifyBackupSourceStopped(t, fixture.api.baseURL, logs)
	t.Logf("客户端 PID %d 退出后，PID %d 只补第 2 片 1 B；原任务 %s、首片 ETag %s、尾片 ETag %s，任务及创建快照各 1 条，已用 0 B、预占 %d B",
		firstPID, resumedPID, first.TaskID, resumed.Receipts[0].ETag, resumed.Receipts[1].ETag, profile.Data.ReservedBytes)
}

func runMultipartClientPhase(t *testing.T, ctx context.Context, fixture backupSourceFixture, directory, phase string) int {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("无法定位联测可执行文件", err)
	}
	command := exec.CommandContext(ctx, executable, "-test.run=^TestMultipartClientRestartChild$", "-test.v", "--", multipartClientRestartArg, phase, directory)
	command.Env = []string{
		"CLOUDNEST_RESTART_CLIENT_SERVER=" + fixture.api.baseURL,
		"CLOUDNEST_S3_PUBLIC_ENDPOINT=" + os.Getenv("CLOUDNEST_S3_PUBLIC_ENDPOINT"),
		"CLOUDNEST_S3_BUCKET=" + fixture.bucket.name,
	}
	command.Stdin = strings.NewReader(os.Getenv("CLOUDNEST_ADMIN_PASSWORD"))
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "--- PASS: TestMultipartClientRestartChild") {
		t.Fatalf("隔离客户端阶段 %s 失败，%v\n%s", phase, err, output)
	}
	t.Logf("隔离客户端 %s 已退出，PID %d\n%s", phase, command.Process.Pid, output)
	return command.Process.Pid
}

func TestMultipartClientRestartChild(t *testing.T) {
	// 1. 仅接受父夹具的专用目录、回环地址和一次性合成密码。
	args := os.Args
	if len(args) < 5 || args[len(args)-4] != "--" || args[len(args)-3] != multipartClientRestartArg {
		t.Skip("仅由客户端重启联测启动")
	}
	phase, directory := args[len(args)-2], args[len(args)-1]
	server := os.Getenv("CLOUDNEST_RESTART_CLIENT_SERVER")
	for _, address := range []string{server, os.Getenv("CLOUDNEST_S3_PUBLIC_ENDPOINT")} {
		endpoint, err := url.Parse(address)
		if err != nil || endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" || endpoint.Port() == "" ||
			endpoint.User != nil || endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			t.Fatal("客户端夹具要求本轮专用回环端点")
		}
	}
	if !filepath.IsAbs(directory) || !strings.HasPrefix(os.Getenv("CLOUDNEST_S3_BUCKET"), "cloudnest-upload-it-") {
		t.Fatal("客户端夹具缺少专用目录或桶")
	}
	password, err := io.ReadAll(io.LimitReader(os.Stdin, 1024))
	if err != nil || len(password) == 0 {
		t.Fatal("读取合成登录凭据失败")
	}
	httpClient := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	defer httpClient.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	api := backupSourceClient{baseURL: server, httpClient: httpClient}
	login := loginBackupFixture(t, ctx, &api, string(password))

	// 2. 首次路径创建一次任务。恢复路径只接收此前写到磁盘的任务记录。
	switch phase {
	case "first-part":
		startMultipartClientUpload(t, ctx, api, directory, login)
	case "resume":
		resumeMultipartClientUpload(t, ctx, api, directory, login)
	default:
		t.Fatal("未知的客户端阶段", phase)
	}
}

func startMultipartClientUpload(t *testing.T, ctx context.Context, api backupSourceClient, directory string, login dto.LoginData) {
	t.Helper()
	// 1. 读取合成源，在正式创建前保存位置、账号、目标、幂等键和源身份。
	payload, err := os.ReadFile(filepath.Join(directory, multipartClientSourceName))
	if err != nil || uint64(len(payload)) != domain.MaxSingleUploadSizeBytes+1 {
		t.Fatal("合成源不是预期的两片样本", err)
	}
	record := multipartClientRestartRecord{
		Server: api.baseURL, UserID: login.User.ID, ParentID: login.RootEntryID, CreateKey: "client-restart-fixture",
		SourceName: multipartClientSourceName, SizeBytes: uint64(len(payload)), Digest: sha256.Sum256(payload),
	}
	writeMultipartClientRecord(t, directory, record)
	var created struct {
		Data      dto.CreateMultipartUploadData `json:"data"`
		RequestID string                        `json:"request_id"`
	}
	declaration := fmt.Sprintf(`{"parent_id":%q,"name":%q,"size_bytes":%d,"sha256":"%x"}`,
		record.ParentID, record.SourceName, record.SizeBytes, record.Digest)
	api.request(t, ctx, http.MethodPost, "/api/v1/uploads", record.CreateKey, strings.NewReader(declaration), http.StatusCreated, &created)
	plan := created.Data
	if created.RequestID == "" || plan.Task.ID == "" || plan.Task.Status != "CREATING" || plan.UploadMode != "MULTIPART" ||
		plan.Task.SizeBytes != record.SizeBytes || plan.PartCount != 2 || plan.PartSizeBytes != domain.MaxSingleUploadSizeBytes {
		t.Fatal("创建回执未交付预期两片计划", plan)
	}
	record.TaskID, record.PartCount, record.PartSizeBytes = plan.Task.ID, plan.PartCount, plan.PartSizeBytes
	writeMultipartClientRecord(t, directory, record)

	// 2. 等待就绪，写入首片并在正常退出前同步回执。
	taskPath := "/api/v1/uploads/" + record.TaskID
	ready := waitForCreateUploadInitialization(t, ctx, api, taskPath)
	if ready.ID != record.TaskID || ready.ParentID != record.ParentID || ready.SizeBytes != record.SizeBytes ||
		!ready.ExpiresAt.Equal(plan.ExpiresAt) {
		t.Fatal("原任务就绪状态不符", ready)
	}
	record.Receipts = []dto.UploadPartReceiptData{putMultipartClientPart(t, ctx, api, taskPath, ready.ExpiresAt, 1, payload[:record.PartSizeBytes])}
	writeMultipartClientRecord(t, directory, record)
}

func resumeMultipartClientUpload(t *testing.T, ctx context.Context, api backupSourceClient, directory string, login dto.LoginData) {
	t.Helper()
	// 1. 用磁盘记录定位原任务，复查账号、计划和已经确认的首片。
	record := readMultipartClientRecord(t, directory)
	if record.Server != api.baseURL || record.UserID != login.User.ID || record.ParentID != login.RootEntryID ||
		record.SourceName != multipartClientSourceName || record.CreateKey == "" || record.TaskID == "" ||
		record.PartCount != 2 || len(record.Receipts) != 1 || record.Receipts[0].PartNumber != 1 {
		t.Fatal("磁盘记录与恢复上下文不符")
	}
	taskPath := "/api/v1/uploads/" + record.TaskID
	var task dto.UploadTaskResponse
	api.request(t, ctx, http.MethodGet, taskPath, "", nil, http.StatusOK, &task)
	if task.RequestID == "" || task.Data.ID != record.TaskID || task.Data.ParentID != record.ParentID ||
		task.Data.RequestedName != record.SourceName || task.Data.Status != "UPLOADING" ||
		task.Data.UploadMode != "MULTIPART" || task.Data.SizeBytes != record.SizeBytes {
		t.Fatal("服务端原任务与持久记录不符", task.Data)
	}
	plan := verifyMultipartClientReceipts(t, ctx, api, record)

	// 2. 重读源文件并验证大小和 SHA-256，仅对缺少回执的尾片签发与上传。
	const missingTail = 2
	tail, err := resumeMultipartFixturePart(filepath.Join(directory, record.SourceName), record.SizeBytes, record.Digest,
		plan, missingTail, func(contents []byte) dto.UploadPartReceiptData {
			return putMultipartClientPart(t, ctx, api, taskPath, task.Data.ExpiresAt, missingTail, contents)
		})
	if err != nil || tail.SizeBytes != 1 {
		t.Fatal("跨进程复检或补传尾片失败", err)
	}
	record.Receipts = append(record.Receipts, tail)
	verifyMultipartClientReceipts(t, ctx, api, record)
	writeMultipartClientRecord(t, directory, record)
}

func verifyMultipartClientReceipts(t *testing.T, ctx context.Context, api backupSourceClient, record multipartClientRestartRecord) dto.UploadPartsData {
	t.Helper()
	var response dto.UploadPartsResponse
	api.request(t, ctx, http.MethodGet, "/api/v1/uploads/"+record.TaskID+"/parts", "", nil, http.StatusOK, &response)
	if response.RequestID == "" || response.Data.PartSizeBytes != record.PartSizeBytes ||
		response.Data.PartCount != record.PartCount || !slices.Equal(response.Data.Parts, record.Receipts) {
		t.Fatal("服务端回执与客户端确认记录不一致", response.Data)
	}
	return response.Data
}

func putMultipartClientPart(t *testing.T, ctx context.Context, api backupSourceClient, taskPath string,
	expiresAt time.Time, number uint32, contents []byte) dto.UploadPartReceiptData {
	t.Helper()
	signedURL, checksum := requestMultipartFixturePartURL(t, ctx, api, taskPath, expiresAt, number, contents)
	if !strings.HasPrefix(signedURL.Path, "/"+os.Getenv("CLOUDNEST_S3_BUCKET")+"/") || signedURL.Query().Get("uploadId") == "" {
		t.Fatal("客户端写入签名未限定到专用桶的分片会话", number)
	}
	etag := putMultipartProbePart(t, ctx, api.httpClient, signedURL, checksum, contents, http.StatusOK, "")
	t.Logf("客户端任务 %s PUT 第 %d 片，%d B，ETag %s", taskPath, number, len(contents), etag)
	return dto.UploadPartReceiptData{PartNumber: number, SizeBytes: uint64(len(contents)), ETag: etag, ChecksumSHA256: checksum}
}

func readMultipartClientRecord(t *testing.T, directory string) multipartClientRestartRecord {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(directory, "multipart-restart.json"))
	if err != nil {
		t.Fatal("读取客户端磁盘记录失败", err)
	}
	var record multipartClientRestartRecord
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal("解析客户端磁盘记录失败", err)
	}
	return record
}

func writeMultipartClientRecord(t *testing.T, directory string, record multipartClientRestartRecord) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(directory, "multipart-restart.json"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal("打开客户端磁盘记录失败", err)
	}
	writeError := json.NewEncoder(file).Encode(record)
	syncError := file.Sync()
	closeError := file.Close()
	if err := errors.Join(writeError, syncError, closeError); err != nil {
		t.Fatal("写入并同步客户端磁盘记录失败", err)
	}
}
