package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestUserProfileDataHTTP(t *testing.T) {
	const maximumSafeBytes uint64 = 1<<53 - 1
	for _, testCase := range []struct {
		name       string
		quotaBytes uint64
		usedBytes  uint64
		wantStatus int
	}{
		{name: "zero counters", quotaBytes: 107374182400, wantStatus: 200},
		{name: "maximum safe bytes", quotaBytes: maximumSafeBytes, usedBytes: maximumSafeBytes, wantStatus: 200},
		{name: "unsafe bytes", quotaBytes: maximumSafeBytes + 1, wantStatus: 500},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 测试专用路由组合现有认证、资料服务和本轮 DTO。
			profileService := service.NewUserProfileService(func(context.Context, uint64) (*domain.UserProfile, error) {
				return &domain.UserProfile{
					ID: ^uint64(0), Username: "fixture", DisplayName: "学习用户", Status: "ACTIVE", RootEntryID: ^uint64(0),
					QuotaBytes: testCase.quotaBytes, UsedBytes: testCase.usedBytes,
				}, nil
			})
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			protected := middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) {
				return &domain.Actor{UserID: ^uint64(0), SessionID: 11, DeviceID: 13}, nil
			}, handler.AuthorizationFailure)
			router.GET("/test-profile-data", protected, func(requestContext *gin.Context) {
				actor, exists := requestcontext.Actor(requestContext.Request.Context())
				if !exists {
					t.Error("资料测试入口缺少 Actor")
					handler.InternalError(requestContext, nil)
					return
				}
				profile, err := profileService.Get(requestContext.Request.Context(), actor.UserID)
				if err != nil {
					handler.AuthorizationFailure(requestContext, err)
					return
				}
				data, err := dto.NewUserProfileData(profile)
				if err != nil {
					handler.InternalError(requestContext, nil)
					return
				}
				requestContext.JSON(http.StatusOK, struct {
					Data      *dto.UserProfileData `json:"data"`
					RequestID string               `json:"request_id"`
				}{Data: data, RequestID: requestcontext.RequestID(requestContext.Request.Context())})
			})
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			request, err := http.NewRequest(http.MethodGet, server.URL+"/test-profile-data", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer fixture-token")
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()

			// 2. 实际解析网络 JSON，核对 ID 类型、数字精度及错误出口。
			decoder := json.NewDecoder(response.Body)
			decoder.UseNumber()
			var body map[string]any
			if err := decoder.Decode(&body); err != nil || response.StatusCode != testCase.wantStatus || len(body) != 2 ||
				response.Header.Get("X-Request-ID") == "" || body["request_id"] != response.Header.Get("X-Request-ID") {
				t.Fatal("网络响应状态、正文或请求 ID 异常")
			}
			if testCase.wantStatus == 500 {
				detail, valid := body["error"].(map[string]any)
				if !valid || detail["code"] != "INTERNAL_ERROR" || response.Header.Get("Cache-Control") != "no-store" {
					t.Fatal("数字越界未进入统一内部错误出口")
				}
				return
			}
			data, valid := body["data"].(map[string]any)
			if !valid || len(data) != 5 {
				t.Fatal("资料数据结构错误")
			}
			user, valid := data["user"].(map[string]any)
			if !valid || len(user) != 3 || user["id"] != "18446744073709551615" || data["root_entry_id"] != "18446744073709551615" || data["reserved_bytes"] != json.Number("0") {
				t.Fatal("完整 ID、用户字段或零字节数输出错误")
			}
			if testCase.usedBytes == maximumSafeBytes && (data["quota_bytes"] != json.Number("9007199254740991") || data["used_bytes"] != json.Number("9007199254740991")) {
				t.Fatal("网络 JSON 丢失安全整数边界值")
			}
		})
	}
}
