package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestReadinessRouteReflectsDependencyChanges(t *testing.T) {
	// 1. 通过服务层模拟依赖故障与恢复，路由器不接触具体客户端。
	databaseFailed, storageFailed := false, false
	checkCount := 0
	healthService := service.NewHealthService(func(ctx context.Context) error {
		checkCount++
		if databaseFailed {
			return errors.New("private-database-credential")
		}
		return nil
	}, func(ctx context.Context) error {
		checkCount++
		if storageFailed {
			return errors.New("private-storage-credential")
		}
		return nil
	})
	router := NewRouter(healthService.Ready)
	testCases := []struct {
		name           string
		databaseFailed bool
		storageFailed  bool
		wantCode       int
		wantStatus     string
	}{
		{name: "healthy", wantCode: http.StatusOK, wantStatus: "ready"},
		{name: "database fails", databaseFailed: true, wantCode: http.StatusServiceUnavailable, wantStatus: "not_ready"},
		{name: "storage fails", storageFailed: true, wantCode: http.StatusServiceUnavailable, wantStatus: "not_ready"},
		{name: "dependencies recover", wantCode: http.StatusOK, wantStatus: "ready"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			databaseFailed, storageFailed = testCase.databaseFailed, testCase.storageFailed
			previousCount := checkCount
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

			// 2. 每次请求重新检查；失败响应不泄漏底层信息，恢复后返回 200。
			var body map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != testCase.wantCode || len(body) != 1 || body["status"] != testCase.wantStatus || checkCount <= previousCount {
				t.Fatalf("status=%d，body=%s，检查次数=%d", response.Code, response.Body, checkCount)
			}
			if response.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
				t.Fatal("响应缺少无缓存约定或 JSON 类型")
			}
			if strings.Contains(response.Body.String(), "private-") {
				t.Fatal("响应泄漏了底层错误")
			}

			// 3. 同一故障期间 live 仍正常响应，而且不执行依赖检查。
			previousCount = checkCount
			liveResponse := httptest.NewRecorder()
			router.ServeHTTP(liveResponse, httptest.NewRequest(http.MethodGet, "/health/live", nil))
			if liveResponse.Code != http.StatusOK || liveResponse.Body.String() != `{"status":"ok"}` || checkCount != previousCount {
				t.Fatal("存活检查被依赖故障影响或访问了依赖")
			}
		})
	}

	// 4. 请求取消沿调用链传递，不再启动依赖检查。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	previousCount := checkCount
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health/ready", nil).WithContext(ctx))
	if response.Code != http.StatusServiceUnavailable || checkCount != previousCount {
		t.Fatal("已取消的请求仍被当作就绪或继续检查依赖")
	}
}
