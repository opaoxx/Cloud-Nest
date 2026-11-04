package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

// startLifecycleServer 创建仅绑定回环临时端口的真实服务，失败时也会清理资源。
func startLifecycleServer(t *testing.T, handler http.Handler, shutdownTimeout time.Duration) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	serveResult := make(chan error, 1)
	stopped := make(chan struct{})
	go func() {
		serveResult <- Serve(ctx, httpServer, shutdownTimeout, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		_ = httpServer.Close()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Error("测试服务未停止")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return address, cancel, serveResult
		}
		select {
		case err := <-serveResult:
			t.Fatalf("测试服务提前退出: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("测试服务未能监听")
	return "", cancel, serveResult
}

func TestServeWaitsForActiveRequest(t *testing.T) {
	// 1. 用同步通知控制一条真实在途请求，避免依靠固定睡眠猜测处理进度。
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	defer func() {
		select {
		case <-releaseRequest:
		default:
			close(releaseRequest)
		}
	}()
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		close(requestStarted)
		<-releaseRequest
		_, _ = io.WriteString(response, "completed")
	})
	address, cancel, serveResult := startLifecycleServer(t, handler, 3*time.Second)
	requestResult := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		response, err := client.Get("http://" + address)
		if err == nil {
			defer response.Body.Close()
			var body []byte
			body, err = io.ReadAll(response.Body)
			if err == nil && (response.StatusCode != http.StatusOK || string(body) != "completed") {
				err = fmt.Errorf("在途响应未完整返回")
			}
		}
		requestResult <- err
	}()
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("请求未进入处理函数")
	}

	// 2. 取消服务后，新连接应被拒绝；在途请求未完成时 Serve 不能提前返回。
	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err != nil {
			break
		}
		_ = connection.Close()
		if time.Now().After(deadline) {
			t.Fatal("停止请求后仍接受新连接")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-serveResult:
		t.Fatalf("在途请求尚未完成，Serve 已返回: %v", err)
	default:
	}

	// 3. 允许处理结束，客户端应收到完整响应，服务随后正常返回。
	close(releaseRequest)
	select {
	case err := <-requestResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("在途请求未结束")
	}
	select {
	case err := <-serveResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("服务未完成优雅退出")
	}
}

func TestServeForcesCloseAfterTimeout(t *testing.T) {
	// 1. 请求只等待自身连接取消，用真实连接验证关闭超时后的行为。
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		close(requestStarted)
		<-request.Context().Done()
		close(requestCanceled)
	})
	address, cancel, serveResult := startLifecycleServer(t, handler, 100*time.Millisecond)
	requestResult := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		response, err := client.Get("http://" + address)
		if response != nil {
			_ = response.Body.Close()
		}
		requestResult <- err
	}()
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("请求未进入处理函数")
	}

	// 2. 超时必须返回期限错误，强制断开连接并通知处理函数取消。
	cancel()
	select {
	case err := <-serveResult:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("预期关闭超时，实际为 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("关闭操作超过测试时限")
	}
	select {
	case <-requestCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("强制关闭后请求上下文未取消")
	}
	if err := <-requestResult; err == nil {
		t.Fatal("超时请求未被断开")
	}
}

func TestServeStartupFailureAndCancellation(t *testing.T) {
	// 1. 持有真实端口，验证监听失败会返回，不等待退出信号。
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	httpServer := &http.Server{Addr: listener.Addr().String()}
	if err := Serve(context.Background(), httpServer, time.Second, logger); err == nil {
		t.Fatal("端口占用时未返回监听错误")
	}

	// 2. 已取消的上下文不应再尝试监听占用端口。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Serve(ctx, httpServer, time.Second, logger); !errors.Is(err, context.Canceled) {
		t.Fatalf("预期取消错误，实际为 %v", err)
	}
}
