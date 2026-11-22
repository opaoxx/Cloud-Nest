package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

const isolatedMultipartBackendArg = "cloudnest-isolated-multipart-backend"
const releaseMultipartVerificationSignal = "v"

func TestIsolatedMultipartBackendChild(t *testing.T) {
	// 1. 只从联测启动此入口，启动服务前确认连接指向本轮回环夹具。
	if len(os.Args) != 5 || os.Args[2] != "--" || os.Args[3] != isolatedMultipartBackendArg {
		t.Skip("仅由分片联测的父进程启动")
	}
	if !strings.HasPrefix(os.Getenv("CLOUDNEST_DB_NAME"), "cloudnest_server_verify_") ||
		!strings.HasPrefix(os.Getenv("CLOUDNEST_S3_BUCKET"), "cloudnest-upload-it-") ||
		os.Getenv("CLOUDNEST_STORAGE_PROVIDER") != "local_minio" {
		t.Fatal("独立后端需要本轮专用库和桶")
	}
	for _, key := range []string{"CLOUDNEST_DB_ADDRESS", "CLOUDNEST_HTTP_ADDR"} {
		host, port, err := net.SplitHostPort(os.Getenv(key))
		if err != nil || host != "127.0.0.1" || port == "" {
			t.Fatal("独立后端需要回环地址", key)
		}
	}
	for _, key := range []string{"CLOUDNEST_S3_ENDPOINT", "CLOUDNEST_S3_PUBLIC_ENDPOINT"} {
		endpoint, err := url.Parse(os.Getenv(key))
		if err != nil || endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" ||
			endpoint.Port() == "" || endpoint.User != nil {
			t.Fatal("独立后端需要回环存储端点", key)
		}
	}

	// 2. 父进程可放行校验观察点，关闭专用管道时按正式上下文流程关停。
	serverContext, cancel := context.WithCancel(t.Context())
	defer cancel()
	logger := logging.New(os.Stdout)
	releaseVerification := make(chan struct{})
	switch os.Args[4] {
	case "normal":
	case "hold-verification":
		// 校验 worker 等父进程放行。观察结束后直接关停时，由取消通知解除阻塞。
		logger = slog.New(&serverWorkerReportGate{
			Handler: logger.Handler(), reportMessage: "分片校验 worker 已启动",
			reportStarted: make(chan struct{}), releaseReport: releaseVerification,
			httpStopped: make(chan struct{}), deadline: serverContext.Done(),
		})
	default:
		t.Fatal("未知的独立服务观察模式")
	}
	go func() {
		defer cancel()
		if os.Args[4] == "hold-verification" {
			var signal [1]byte
			if _, err := io.ReadFull(os.Stdin, signal[:]); err != nil {
				return
			}
			if string(signal[:]) != releaseMultipartVerificationSignal {
				t.Error("独立后端收到无效的联测放行信号")
				return
			}
			close(releaseVerification)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
	}()
	if err := run(serverContext, logger); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal("独立后端运行失败", err)
	}
}

func startIsolatedMultipartBackend(t *testing.T, ctx context.Context, databaseName string) (backupSourceClient, func() (string, error), int) {
	t.Helper()
	address, stop, pid, _ := launchIsolatedMultipartBackend(t, ctx, databaseName, "normal")
	readyContext, cancelReady := context.WithTimeout(ctx, 10*time.Second)
	defer cancelReady()
	return waitForBackupFixtureClient(t, readyContext, address), stop, pid
}

func launchIsolatedMultipartBackend(t *testing.T, ctx context.Context, databaseName, mode string) (string, func() (string, error), int, func() error) {
	t.Helper()
	// 1. 从当前测试可执行文件启动专用后端进程，继承已建立的隔离连接。
	address := configureBackupFixtureServer(t, databaseName)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal("无法定位联测可执行文件", err)
	}
	processContext, terminate := context.WithCancel(ctx)
	t.Cleanup(terminate)
	command := exec.CommandContext(processContext, executable, "-test.run=^TestIsolatedMultipartBackendChild$", "--", isolatedMultipartBackendArg, mode)
	command.WaitDelay = 5 * time.Second
	stopInput, err := command.StdinPipe()
	if err != nil {
		t.Fatal("无法建立独立后端的关闭管道", err)
	}
	var logs bytes.Buffer
	command.Stdout, command.Stderr = &logs, &logs
	if err := command.Start(); err != nil {
		_ = stopInput.Close()
		t.Fatal("无法启动独立后端", err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = command.Wait()
		close(done)
	}()

	// 2. 正常退出先关闭输入并等待，超时才终止本轮创建的进程。
	stop := func() (string, error) {
		_ = stopInput.Close()
		select {
		case <-done:
			return logs.String(), waitErr
		case <-time.After(35 * time.Second):
			terminate()
			<-done
			return logs.String(), fmt.Errorf("独立后端未在期限内正常退出")
		}
	}
	t.Cleanup(func() {
		if _, err := stop(); err != nil {
			t.Error("独立后端退出失败", err)
		}
	})
	release := func() error {
		_, err := io.WriteString(stopInput, releaseMultipartVerificationSignal)
		return err
	}
	return address, stop, command.Process.Pid, release
}

type largeMultipartBackendMemory struct {
	workingSet, peakWorkingSet, peakCommit uint64
}

func readLargeMultipartBackendMemory(t *testing.T, ctx context.Context, pid int) largeMultipartBackendMemory {
	t.Helper()
	// Windows 维护进程生命周期的两个高水位，请求之间无需采样即可保留瞬时峰值。
	script := fmt.Sprintf("$backend = Get-Process -Id %d -ErrorAction Stop\n"+
		"'{0} {1} {2}' -f $backend.WorkingSet64, $backend.PeakWorkingSet64, $backend.PeakPagedMemorySize64", pid)
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	output, err := command.Output()
	if err != nil {
		t.Fatal("读取独立后端的进程内存失败", pid, err)
	}
	var memory largeMultipartBackendMemory
	if _, err := fmt.Sscan(string(output), &memory.workingSet, &memory.peakWorkingSet, &memory.peakCommit); err != nil ||
		memory.workingSet == 0 || memory.peakWorkingSet < memory.workingSet || memory.peakCommit == 0 {
		t.Fatal("独立后端的内存计数无效", pid, err)
	}
	return memory
}

func verifyLargeMultipartBackendMemory(t *testing.T, pid int, before, after largeMultipartBackendMemory) {
	t.Helper()
	// 256 MiB 是本夹具用于捕获全文件缓冲回归的阈值。
	const maxPeakBytes = 256 << 20
	if after.peakWorkingSet < before.peakWorkingSet || after.peakCommit < before.peakCommit ||
		after.peakWorkingSet > maxPeakBytes || after.peakCommit > maxPeakBytes {
		t.Fatalf("原任务校验所处后端进程的内存超出夹具边界，PID %d，提交前 %+v，发布后 %+v，阈值 %d B",
			pid, before, after, maxPeakBytes)
	}
	t.Logf("独立后端内存 PID %d，完成前工作集 %d B、峰值 %d B、提交量峰值 %d B，发布后工作集 %d B、峰值 %d B、提交量峰值 %d B，夹具阈值 %d B",
		pid, before.workingSet, before.peakWorkingSet, before.peakCommit,
		after.workingSet, after.peakWorkingSet, after.peakCommit, maxPeakBytes)
}
