package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"golang.org/x/crypto/bcrypt"
)

func TestServerWebSingleUploadContentIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_WEB_UPLOAD_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 建立独占存储和生产路由实例。
	pool, bucket, webURL := prepareWebSingleUploadFixture(t, ctx)

	// 2. 在浏览器验证原始正文、零字节、同任务 202、连点和超时后的提示。
	runWebBrowserProbe(t, ctx, cli, webURL, "web_upload_content.js", "CN524 browser checks passed")
	t.Log("Edge 验收通过：普通文件与空文件各一次 PUT 和 202，模拟 408 后保留任务 ID 且没有重传")

	// 3. 从专用桶读回对象，逐字节核对浏览器发送的内容。后台可能已推进至 COMPLETED。
	for _, fixture := range []struct {
		name string
		body []byte
	}{
		{"隔离二进制.bin", []byte{0, 255, 17, 128, 10, 42}},
		{"隔离空文件.bin", nil},
	} {
		var key string
		if err := pool.QueryRowContext(ctx, `SELECT o.object_key FROM upload_tasks t
			JOIN stored_objects o ON o.id=t.staged_object_id AND o.owner_user_id=t.user_id
			WHERE t.user_id=7 AND t.target_parent_id=33 AND t.requested_name=? AND t.file_size=?
			AND t.status IN ('VERIFYING','COMPLETED') AND t.lease_version>=1 AND o.bucket=?`,
			fixture.name, len(fixture.body), bucket.name).Scan(&key); err != nil {
			t.Fatal("接收后的任务与暂存对象记录不符", err)
		}
		object, err := bucket.client.GetObject(ctx, bucket.name, key, minio.GetObjectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		actual, readErr := io.ReadAll(io.LimitReader(object, int64(len(fixture.body)+1)))
		closeErr := object.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(actual, fixture.body) {
			t.Fatal("从隔离 MinIO 读回的字节与浏览器选中的合成文件不一致", fixture.name, readErr, closeErr)
		}
	}
	versions := 0
	for object := range bucket.client.ListObjects(ctx, bucket.name, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
		if object.Err != nil {
			t.Fatal(object.Err)
		}
		versions++
	}
	var tasks, unsent, accounted int
	if err := pool.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM upload_tasks),
		(SELECT COUNT(*) FROM upload_tasks WHERE user_id=7 AND requested_name='隔离超时.bin'
		 AND status='UPLOADING' AND lease_version=0 AND reserved_bytes=3),
		(SELECT COUNT(*) FROM users WHERE id=7 AND used_storage+reserved_storage=9)`).Scan(&tasks, &unsent, &accounted); err != nil {
		t.Fatal(err)
	}
	if versions != 2 || tasks != 3 || unsent != 1 || accounted != 1 {
		t.Fatalf("接收结果不符，对象版本=%d 任务=%d 未发送任务=%d 配额核对=%d", versions, tasks, unsent, accounted)
	}
	t.Log("MinIO 读回 6 字节和 0 字节均一致，对象版本 2 个，模拟超时任务仍为 UPLOADING")
}

func prepareWebSingleUploadFixture(t *testing.T, ctx context.Context) (*sql.DB, *serverUploadContentBucket, string) {
	t.Helper()
	startIsolatedWebMySQL(t, ctx)
	startIsolatedWebMinIO(t, ctx)
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	bucket := prepareServerUploadContentBucket(t, ctx)
	hash, err := bcrypt.GenerateFromPassword([]byte(webLoginFixturePassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash,storage_quota) VALUES(7,?,?,?,1024)",
		webLoginFixtureUsername, webLoginFixtureDisplayName, string(hash)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(33,7,'',X'','DIR')"); err != nil {
		t.Fatal(err)
	}
	return pool, bucket, startServerWebFixture(t, ctx, databaseName, run)
}

// startIsolatedWebMinIO 为本轮联测启动只映射回环地址的独占 MinIO。
func startIsolatedWebMinIO(t *testing.T, ctx context.Context) string {
	t.Helper()
	return startIsolatedWebMinIOWithDataSize(t, ctx, "128m")
}

func startIsolatedWebMinIOWithDataSize(t *testing.T, ctx context.Context, dataSize string) string {
	t.Helper()
	requireLocalWebFixtureDocker(t, ctx)
	const image = "sha256:7c840b89c21e0af0e824cf57310becadbbd45c7fb407c78cd2ada4b74c76f733"
	accessKey, secretKey := rand.Text(), rand.Text()
	output, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "--pull", "never",
		"--tmpfs", "/data:uid=10001,gid=10001,mode=0700,size="+dataSize, "--publish", "127.0.0.1::9000",
		"-e", "MINIO_ROOT_USER="+accessKey, "-e", "MINIO_ROOT_PASSWORD="+secretKey, image).Output()
	containerID := strings.TrimSpace(string(output))
	if err != nil || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(containerID) {
		t.Fatal("无法创建本轮专用 MinIO 容器", err)
	}
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 25*time.Second)
		defer stop()
		if err := exec.CommandContext(cleanupContext, "docker", "stop", containerID).Run(); err != nil {
			t.Error("隔离 MinIO 容器未正常停止", err)
		}
	})
	portOutput, err := exec.CommandContext(ctx, "docker", "port", containerID, "9000/tcp").Output()
	address := strings.TrimSpace(string(portOutput))
	if err != nil || !regexp.MustCompile(`^127\.0\.0\.1:\d+$`).MatchString(address) {
		t.Fatal("未取得本轮 MinIO 的回环端口", err)
	}
	readyContext, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	for {
		probe, err := http.NewRequestWithContext(readyContext, http.MethodGet, "http://"+address+"/minio/health/ready", nil)
		if err != nil {
			t.Fatal(err)
		}
		if response, err := client.Do(probe); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-readyContext.Done():
			t.Fatal("隔离 MinIO 未在期限内就绪")
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Setenv("CLOUDNEST_S3_ENDPOINT", "http://"+address)
	t.Setenv("CLOUDNEST_S3_PUBLIC_ENDPOINT", "http://"+address)
	t.Setenv("CLOUDNEST_S3_BUCKET", "cloudnest-web-upload-test")
	t.Setenv("CLOUDNEST_S3_ACCESS_KEY", accessKey)
	t.Setenv("CLOUDNEST_S3_SECRET_KEY", secretKey)
	return containerID
}
