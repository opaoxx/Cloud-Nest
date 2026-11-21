package main

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestServerWebPurgeConfirmationIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_WEB_PURGE_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 启动本轮专用 MySQL，再复用合成子树、只读存储桩和停用清理 worker 的实例。
	startIsolatedWebMySQL(t, ctx)
	pool, webURL := prepareWebPurgePreviewFixture(t, ctx)
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
		VALUES(60,7,33,'跨页恢复目录',CAST('跨页恢复目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))`); err != nil {
		t.Fatal(err)
	}
	before := readWebPurgePreviewSnapshot(t, ctx, pool)

	// 2. 在独立 Edge 会话中验证交互、请求次数和提交使用的原预检。
	runWebBrowserProbe(t, ctx, cli, webURL, "web_purge_confirmation.js", "CN520 and CN521 browser checks passed")
	t.Log("Edge 验收通过：同一范围、取消焦点、取消、Esc、Enter、过期、跨页恢复、原凭证和一次 202")
	t.Log("进度验收通过：真实受理地址、加载、清理中 4 项、读取失败和重试、无效状态拒绝、模拟完成 0 项；五次 GET")

	// 3. 确认只有受理根项和刻意恢复的对照项发生变化，对象和配额保持原样。
	var accepted, restored, writes int
	if err := pool.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM file_entries WHERE id=56 AND status='PURGING' AND revision=2 AND purge_started_at IS NOT NULL AND purged_at IS NULL),
		(SELECT COUNT(*) FROM file_entries WHERE id=60 AND status='ACTIVE' AND revision=2 AND deleted_at IS NULL AND purge_started_at IS NULL),
		(SELECT COUNT(*) FROM idempotency_records)`).Scan(&accepted, &restored, &writes); err != nil {
		t.Fatal(err)
	}
	if accepted != 1 || restored != 1 || writes != 2 {
		t.Fatalf("预期一次确认及一次对照恢复，实际受理=%d 恢复=%d 写入记录=%d", accepted, restored, writes)
	}
	unchanged := func(snapshot []string) []string {
		return slices.DeleteFunc(snapshot, func(row string) bool {
			return strings.HasPrefix(row, `["entry", 56,`) || strings.HasPrefix(row, `["entry", 60,`) ||
				strings.HasPrefix(row, `["idempotency_count",`)
		})
	}
	if after := readWebPurgePreviewSnapshot(t, ctx, pool); !slices.Equal(unchanged(after), unchanged(before)) {
		t.Fatal("受理及对照恢复之外的合成业务数据发生变化")
	}
	t.Log("数据库核对通过：56 进入 PURGING，60 恢复为 ACTIVE，其余条目、对象和配额保持原样")
}

func webFixtureBrowserCLI(t *testing.T, envKey string) string {
	t.Helper()
	cli := os.Getenv(envKey)
	if cli == "" {
		t.Skipf("设置 %s 指向本地 playwright-cli.js 后进行隔离浏览器验收", envKey)
	}
	if !filepath.IsAbs(cli) {
		t.Fatal("Playwright CLI 必须是绝对路径")
	}
	if _, err := os.Stat(cli); err != nil {
		t.Fatal(err)
	}
	return cli
}

func runWebBrowserProbe(t *testing.T, ctx context.Context, cli, webURL, filename, successMarker string) string {
	t.Helper()
	probe, err := filepath.Abs(filepath.Join("testdata", filename))
	if err != nil {
		t.Fatal(err)
	}
	cliDir := t.TempDir()
	session := "-s=cloudnest-fixture-" + strings.ToLower(rand.Text())
	runBrowser := func(runContext context.Context, args ...string) (string, error) {
		cmd := exec.CommandContext(runContext, "node", append([]string{cli, session}, args...)...)
		cmd.Dir = cliDir
		output, err := cmd.CombinedOutput()
		return string(output), err
	}
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if _, err := runBrowser(cleanupContext, "close"); err != nil {
			t.Error("隔离浏览器会话未正常关闭", err)
		}
	})
	if output, err := runBrowser(ctx, "open", webURL, "--browser=msedge"); err != nil || strings.Contains(output, "### Error") {
		t.Fatalf("启动隔离 Edge 失败，%v\n%s", err, output)
	}
	output, err := runBrowser(ctx, "run-code", "--filename", probe)
	if err != nil || strings.Contains(output, "### Error") || !strings.Contains(output, successMarker) {
		t.Fatalf("浏览器验收失败，%v\n%s", err, output)
	}
	return cliDir
}

func startIsolatedWebMySQL(t *testing.T, ctx context.Context) {
	t.Helper()
	// 1. 仅允许本机 Docker，数据库地址和凭据由这次启动给出。
	if os.Getenv("DOCKER_HOST") != "" {
		t.Fatal("请在未覆盖 DOCKER_HOST 的本机 Docker 上执行此隔离测试")
	}
	endpoint, err := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil || (!strings.HasPrefix(string(endpoint), "npipe://") && !strings.HasPrefix(string(endpoint), "unix://")) {
		t.Fatal("未确认 Docker 上下文使用本机套接字", err)
	}
	const image = "sha256:a81d043930e51745522bd5c430961cf8286f0a23f0a9a2a6e0108f7ea71b0902"
	password := rand.Text()
	output, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "--pull", "never",
		"--tmpfs", "/var/lib/mysql", "--publish", "127.0.0.1::3306", "-e", "MYSQL_ROOT_PASSWORD="+password, image).Output()
	containerID := strings.TrimSpace(string(output))
	if err != nil || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(containerID) {
		t.Fatal("无法创建本轮专用 MySQL 容器", err)
	}
	t.Cleanup(func() {
		cleanupContext, stop := context.WithTimeout(context.Background(), 25*time.Second)
		defer stop()
		if err := exec.CommandContext(cleanupContext, "docker", "stop", containerID).Run(); err != nil {
			t.Error("隔离 MySQL 容器未正常停止", err)
		}
	})

	// 2. 容器内就绪后验证回环端口，并覆盖测试进程的数据库连接信息。
	readyContext, stop := context.WithTimeout(ctx, 60*time.Second)
	defer stop()
	for exec.CommandContext(readyContext, "docker", "exec", "-e", "MYSQL_PWD="+password, containerID,
		"mysqladmin", "--protocol=tcp", "--host=127.0.0.1", "--user=root", "status").Run() != nil {
		select {
		case <-readyContext.Done():
			t.Fatal("隔离 MySQL 未在期限内就绪")
		case <-time.After(time.Second):
		}
	}
	portOutput, err := exec.CommandContext(ctx, "docker", "port", containerID, "3306/tcp").Output()
	address := strings.TrimSpace(string(portOutput))
	if err != nil || !regexp.MustCompile(`^127\.0\.0\.1:\d+$`).MatchString(address) {
		t.Fatal("未取得本轮 MySQL 的回环端口", err)
	}
	t.Setenv("CLOUDNEST_DB_ADDRESS", address)
	t.Setenv("CLOUDNEST_DB_NAME", "information_schema")
	t.Setenv("CLOUDNEST_DB_USER", "root")
	t.Setenv("CLOUDNEST_DB_PASSWORD", password)
}
