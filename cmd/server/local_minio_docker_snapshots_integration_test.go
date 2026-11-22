package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLocalMinIODockerSnapshotReaderIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_MINIO_DOCKER_SNAPSHOT_TEST") != "1" {
		t.Skip("显式启用后仅检查本轮专用 MinIO 容器")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	if os.Getenv("DOCKER_HOST") != "" || os.Getenv("DOCKER_CONTEXT") != "" {
		t.Fatal("隔离测试需要本机 Docker Desktop 默认连接")
	}
	host, err := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil || strings.TrimSpace(string(host)) != localDockerDesktopHost {
		t.Fatal("未确认本机 Docker Desktop Linux 引擎", err)
	}
	project := "cloudnest-proof-fixture-" + uuid.NewString()
	projectLabel := "com.docker.compose.project=" + project
	const serviceLabel = "com.docker.compose.service=minio"
	read := newLocalMinIODockerSnapshotReader(project)
	verify := newLocalMinIOSessionLockVerifier(project, read)
	if snapshots, err := read(ctx); err != nil || len(snapshots) != 0 {
		t.Fatal("本轮项目在启动前应没有容器", err)
	}

	// 1. 仅启动本轮标记的容器，并为重启、替换预留同一个回环端口。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + address
	startFixture := func() (string, func()) {
		t.Helper()
		output, err := runLocalDockerCommand(ctx, "run", "-d", "--pull", "never", "--network", "bridge",
			"--label", projectLabel, "--label", serviceLabel, "--tmpfs", "/data:uid=10001,gid=10001,mode=0700,size=64m",
			"--publish", address+":9000", "-e", "MINIO_ROOT_USER="+uuid.NewString(), "-e", "MINIO_ROOT_PASSWORD="+uuid.NewString(), verifiedLocalMinIOImageID)
		id := strings.TrimSpace(string(output))
		if err != nil || !localMinIOContainerIDPattern.MatchString(id) {
			t.Fatal("未启动本轮专用容器", err)
		}
		t.Logf("专用 MinIO %s，回环地址 %s", id[:12], endpoint)
		removed := false
		remove := func() {
			t.Helper()
			if removed {
				return
			}
			cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			label, err := runLocalDockerCommand(cleanup, "container", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, id)
			if err != nil || strings.TrimSpace(string(label)) != project {
				t.Error("拒绝清理归属未核实的容器", err)
				return
			}
			if _, err := runLocalDockerCommand(cleanup, "container", "rm", "--force", id); err != nil {
				t.Error("专用容器清理失败", err)
				return
			}
			left, err := runLocalDockerCommand(cleanup, "container", "ls", "--all", "--filter", "id="+id, "--format", "{{.ID}}")
			if err != nil || strings.TrimSpace(string(left)) != "" {
				t.Error("专用容器未确认移除", err)
				return
			}
			removed = true
			t.Logf("专用容器已移除 %s", id[:12])
		}
		t.Cleanup(remove)
		return id, remove
	}
	readIdentity := func(wantID string) string {
		t.Helper()
		identity, err := verify(ctx, endpoint)
		if err != nil || !strings.HasPrefix(identity, wantID+"/") {
			t.Fatalf("专用 MinIO %s 未获得预期证明，错误 %v", wantID[:12], err)
		}
		return identity
	}

	// 2. 读取真实运行元数据，验证同一次运行的证明稳定。
	original, removeOriginal := startFixture()
	first := readIdentity(original)
	if first != readIdentity(original) {
		t.Fatal("同一次运行的身份变化")
	}
	otherProject := newLocalMinIODockerSnapshotReader(project + "-other")
	if snapshots, err := otherProject(ctx); err != nil || len(snapshots) != 0 {
		t.Fatal("不同 Compose 项目读到了本轮容器", err)
	}

	// 3. 真实重启并替换，校验当前身份和端口映射均来自新快照。
	if _, err := runLocalDockerCommand(ctx, "container", "restart", original); err != nil {
		t.Fatal("专用容器重启失败", err)
	}
	restarted := readIdentity(original)
	if restarted == first {
		t.Fatal("重启仍返回旧启动时间")
	}
	removeOriginal()
	if identity, err := verify(ctx, endpoint); err != nil || identity != "" {
		t.Fatal("已移除容器仍能交付证明", err)
	}
	replacement, _ := startFixture()
	if identity := readIdentity(replacement); replacement == original || identity == restarted {
		t.Fatal("同地址替换仍返回旧身份")
	}
	t.Log("项目标签、真实命令、镜像与端口均通过；重启换启动时间，同端口替换换容器 ID")
}
