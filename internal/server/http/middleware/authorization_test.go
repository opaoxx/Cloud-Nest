package middleware

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestRequireAuthorizationDependencies(t *testing.T) {
	for _, missingAuthorize := range []bool{true, false} {
		t.Run(map[bool]string{true: "missing authorize", false: "missing reject"}[missingAuthorize], func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("缺少依赖应暴露装配错误")
				}
			}()
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, domain.ErrInvalidAccessToken }
			reject := func(*gin.Context, error) {}
			if missingAuthorize {
				authorize = nil
			} else {
				reject = nil
			}
			RequireAuthorization(authorize, reject)
		})
	}
}

func TestRequireAuthorizationAlwaysAbortsRejection(t *testing.T) {
	for _, failure := range []string{"header", "service", "identity", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			rejections := 0
			protected := RequireAuthorization(func(context.Context, string) (*domain.Actor, error) {
				if failure == "service" {
					return nil, domain.ErrInvalidAccessToken
				}
				return nil, nil
			}, func(requestContext *gin.Context, _ error) {
				rejections++
				requestContext.Status(401)
			})
			router := gin.New()
			router.GET("/test", protected, func(*gin.Context) { t.Error("拒绝后进入了下游") })
			request := httptest.NewRequest("GET", "/test", nil)
			if failure != "header" {
				request.Header.Set("Authorization", "Bearer test-token")
			}
			if failure == "cancellation" {
				ctx, cancel := context.WithCancel(request.Context())
				cancel()
				request = request.WithContext(ctx)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != 401 || rejections != 1 {
				t.Fatal("拒绝应恰好执行一次失败响应")
			}
		})
	}
}
