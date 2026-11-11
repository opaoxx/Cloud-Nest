package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
)

// storageTestConfig 使用本机模拟地址和固定测试凭证，不依赖用户环境变量。
func storageTestConfig(t *testing.T, endpoint string) config.StorageConfig {
	t.Helper()
	endpointURL, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return config.StorageConfig{Provider: "local_minio", EndpointURL: endpointURL, PublicEndpointURL: endpointURL, Bucket: "cloudnest", AccessKey: "test-access", SecretKey: "credential-must-not-be-logged"}
}

func TestNewClientS3Responses(t *testing.T) {
	// 1. 模拟只读 S3 元数据响应，拒绝任何对象操作或写入请求。
	testCases := []struct {
		name          string
		status        int
		regionFailure bool
		wantError     string
	}{
		{name: "existing bucket", status: http.StatusOK},
		{name: "missing bucket", status: http.StatusNotFound, wantError: "桶不存在"},
		{name: "access denied", status: http.StatusForbidden, wantError: "HTTP 403"},
		{name: "server message not exposed", status: http.StatusServiceUnavailable, regionFailure: true, wantError: "HTTP 503"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var requestCount atomic.Int32
			connectionClosed := make(chan struct{}, 8)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				requestCount.Add(1)
				if request.URL.Path != "/cloudnest/" && request.URL.Path != "/cloudnest" {
					t.Error("检查请求访问了桶以外的路径")
				}
				if !strings.HasPrefix(request.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
					t.Error("缺少 V4 签名，检查不应匿名访问")
				}
				if request.Method == http.MethodGet && request.URL.Query().Has("location") {
					response.Header().Set("Content-Type", "application/xml")
					if testCase.regionFailure {
						response.WriteHeader(testCase.status)
						_, _ = io.WriteString(response, "<Error><Code>ServiceUnavailable</Code><Message>credential-must-not-be-logged</Message></Error>")
						return
					}
					_, _ = io.WriteString(response, "<LocationConstraint>us-east-1</LocationConstraint>")
					return
				}
				if request.Method != http.MethodHead || len(request.URL.Query()) != 0 {
					t.Error("桶检查发送了非只读元数据请求")
				}
				response.WriteHeader(testCase.status)
			}))
			server.Config.ConnState = func(connection net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					select {
					case connectionClosed <- struct{}{}:
					default:
					}
				}
			}
			server.Start()
			t.Cleanup(server.Close)

			// 2. 调用真实 SDK，错误不返回客户端，也不回显服务端消息。
			client, err := NewClient(context.Background(), storageTestConfig(t, server.URL))
			if testCase.wantError != "" {
				if client != nil {
					client.Close()
					t.Fatal("检查失败仍返回了客户端")
				}
				if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
					t.Fatalf("预期错误包含 %q，实际为 %v", testCase.wantError, err)
				}
				if strings.Contains(err.Error(), "credential-must-not-be-logged") {
					t.Fatal("错误回显了服务端原始消息")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(client.Close)
				previousCount := requestCount.Load()
				if err := client.CheckBucket(context.Background()); err != nil || requestCount.Load() <= previousCount {
					t.Fatalf("复查没有实际请求存储，error=%v", err)
				}
				canceledContext, cancel := context.WithCancel(context.Background())
				cancel()
				if err := client.CheckBucket(canceledContext); !errors.Is(err, context.Canceled) {
					t.Fatalf("未保留取消语义，error=%v", err)
				}
				client.Close()
			}

			// 3. 成功后的显式关闭与失败后的自动清理都必须释放连接。
			select {
			case <-connectionClosed:
			case <-time.After(2 * time.Second):
				t.Fatal("客户端未释放与模拟服务的连接")
			}
		})
	}
}

func TestNewClientTimeoutAndCancellation(t *testing.T) {
	// 1. 已取消的上下文不应创建客户端或发出请求。
	canceledContext, cancel := context.WithCancel(context.Background())
	cancel()
	if client, err := NewClient(canceledContext, storageTestConfig(t, "http://127.0.0.1:19000")); client != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("已取消的创建操作未立即失败")
	}

	// 2. 模拟不返回响应的存储服务，覆盖更短期限与默认 5 秒期限。
	for _, callerTimeout := range []time.Duration{100 * time.Millisecond, 10 * time.Second} {
		t.Run(callerTimeout.String(), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				<-request.Context().Done()
			}))
			defer server.Close()
			checkContext, cancel := context.WithTimeout(context.Background(), callerTimeout)
			defer cancel()
			startedAt := time.Now()
			client, err := NewClient(checkContext, storageTestConfig(t, server.URL))
			elapsed := time.Since(startedAt)
			if client != nil {
				client.Close()
				t.Fatal("超时后仍返回了客户端")
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("未保留超时语义，error=%v", err)
			}
			if callerTimeout == 100*time.Millisecond && elapsed > 2*time.Second {
				t.Fatalf("调用方期限未及时生效，elapsed=%s", elapsed)
			}
			if callerTimeout == 10*time.Second && (elapsed < 4*time.Second || elapsed > 8*time.Second) {
				t.Fatalf("默认 5 秒期限未生效，elapsed=%s", elapsed)
			}
		})
	}
}

func TestCheckBucketTransportTimeout(t *testing.T) {
	// 1. 先建立可用客户端，再让模拟服务停止返回响应头。
	var stall atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if stall.Load() {
			<-request.Context().Done()
			return
		}
		if request.URL.Query().Has("location") {
			_, _ = io.WriteString(response, "<LocationConstraint>us-east-1</LocationConstraint>")
			return
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, err := NewClient(context.Background(), storageTestConfig(t, server.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// 当前没有在途调用，缩短本用例的传输层期限，稳定复现先于上下文到期的路径。
	client.transport.ResponseHeaderTimeout = 20 * time.Millisecond
	stall.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// 2. 传输层超时也要保留统一超时类型，错误文本使用固定本地提示。
	err = client.CheckBucket(ctx)
	if ctx.Err() != nil {
		t.Fatal("未在调用方期限前复现传输层超时")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("传输层超时类型丢失 error=%v", err)
	}
	if strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "credential-must-not-be-logged") {
		t.Fatal("超时错误回显了地址或凭证")
	}
}

func TestMinIOIntegration(t *testing.T) {
	// 1. 仅在显式启用时访问配置中的已有桶，不创建桶或对象。
	if os.Getenv("CLOUDNEST_MINIO_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_MINIO_TEST=1 后执行已有 MinIO 桶的只读集成验证")
	}
	storageConfig, err := config.LoadStorageConfig()
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(context.Background(), storageConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.CheckBucket(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 2. 错误凭证和随机不存在的桶必须失败，不能自动创建或匿名回退。
	wrongCredentials := storageConfig
	wrongCredentials.SecretKey = "wrong-secret-for-minio-check"
	if invalidClient, err := NewClient(context.Background(), wrongCredentials); invalidClient != nil || err == nil {
		if invalidClient != nil {
			invalidClient.Close()
		}
		t.Fatal("错误凭证未被拒绝")
	}
	missingBucket := storageConfig
	missingBucket.Bucket = fmt.Sprintf("cloudnest-missing-%d", time.Now().UnixNano())
	if invalidClient, err := NewClient(context.Background(), missingBucket); invalidClient != nil || err == nil || !strings.Contains(err.Error(), "桶不存在") {
		if invalidClient != nil {
			invalidClient.Close()
		}
		t.Fatalf("不存在的桶未返回预期错误，error=%v", err)
	}
}
