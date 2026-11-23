package http

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
)

func TestRegisterSyncSnapshotPageRoute(t *testing.T) {
	// 1. 在同一独立路由器注册三条快照路由，核对回调收到的身份和输入。
	const snapshotCollection = "/api/v1/sync/snapshots"
	const cursor = "request-payload.request-signature"
	const baseline = "baseline-payload.baseline-signature"
	const key = "Fixture-Create-Snapshot"
	page := syncSnapshotPageSuccessFixture()
	snapshotID := page.SnapshotID.String()
	snapshotPath := snapshotCollection + "/" + snapshotID
	confirmPath := snapshotPath + "/confirm"
	actor := domain.Actor{UserID: 7, SessionID: 17, DeviceID: 19}
	var authorizationCalls, pageCalls, createCalls, confirmCalls int
	router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	authorize := func(_ context.Context, token string) (*domain.Actor, error) {
		authorizationCalls++
		if token == "expired-fixture-access" {
			return nil, domain.ErrAccessTokenExpired
		}
		if token != "fixture-access" {
			t.Errorf("收到意外的 Bearer 值 %q", token)
		}
		return &actor, nil
	}
	RegisterSyncSnapshotPageRoute(router, authorize, func(ctx context.Context, receivedActor domain.Actor, receivedID uuid.UUID, receivedCursor string) (*domain.SyncSnapshotPageResult, error) {
		pageCalls++
		storedActor, exists := requestcontext.Actor(ctx)
		if authorizationCalls != 1 || !exists || storedActor != actor || receivedActor != actor ||
			receivedID != page.SnapshotID || receivedCursor != cursor || requestcontext.RequestID(ctx) == "" {
			t.Error("续页回调未收到已认证身份、准确的分页参数或请求 ID")
		}
		return page, nil
	})
	RegisterCreateSyncSnapshotRoute(router, authorize, func(ctx context.Context, receivedActor domain.Actor, receivedKey string, pageSize int) (*domain.SyncSnapshotPageResult, error) {
		createCalls++
		storedActor, exists := requestcontext.Actor(ctx)
		if authorizationCalls != 1 || !exists || storedActor != actor || receivedActor != actor ||
			receivedKey != key || pageSize != 1 || requestcontext.RequestID(ctx) == "" {
			t.Error("创建回调未收到已认证身份、准确的创建参数或请求 ID")
		}
		return page, nil
	})
	RegisterConfirmSyncSnapshotBaselineRoute(router, authorize, func(ctx context.Context, receivedActor domain.Actor, receivedID uuid.UUID, receivedToken string) (uint64, error) {
		confirmCalls++
		storedActor, exists := requestcontext.Actor(ctx)
		if authorizationCalls != 1 || !exists || storedActor != actor || receivedActor != actor ||
			receivedID != page.SnapshotID || receivedToken != baseline || requestcontext.RequestID(ctx) == "" {
			t.Error("确认回调未收到已认证身份、准确的路径、凭据或请求 ID")
		}
		return 17, nil
	})

	// 2. 区分成功、认证拒绝、参数拒绝及未匹配路由，检查每一步的调用次数。
	for _, testCase := range []struct {
		name, method, path, bearer                  string
		body                                        string
		hasKey                                      bool
		status, authCalls, reads, creates, confirms int
		errorCode, field                            string
	}{
		{name: "read page", method: http.MethodGet, path: snapshotPath + "?cursor=" + cursor, bearer: "fixture-access", status: 200, authCalls: 1, reads: 1},
		{name: "decoded path and cursor", method: http.MethodGet, path: snapshotCollection + "/%30" + snapshotID[1:] + "?cursor=request-payload%2Erequest-signature", bearer: "fixture-access", status: 200, authCalls: 1, reads: 1},
		{name: "missing Bearer before invalid parameters", method: http.MethodGet, path: snapshotCollection + "/bad-id", status: 401, errorCode: "AUTH_REQUIRED"},
		{name: "expired Bearer before invalid parameters", method: http.MethodGet, path: snapshotCollection + "/bad-id", bearer: "expired-fixture-access", status: 401, authCalls: 1, errorCode: "TOKEN_EXPIRED"},
		{name: "invalid path ID", method: http.MethodGet, path: snapshotCollection + "/bad-id?cursor=" + cursor, bearer: "fixture-access", status: 400, authCalls: 1, errorCode: "INVALID_ARGUMENT", field: "snapshot_id"},
		{name: "missing cursor", method: http.MethodGet, path: snapshotPath, bearer: "fixture-access", status: 400, authCalls: 1, errorCode: "INVALID_CURSOR", field: "cursor"},
		{name: "wrong method", method: http.MethodPost, path: snapshotPath + "?cursor=" + cursor, bearer: "fixture-access", status: 404, errorCode: "ROUTE_NOT_FOUND"},
		{name: "wrong path", method: http.MethodGet, path: snapshotCollection, bearer: "fixture-access", status: 404, errorCode: "ROUTE_NOT_FOUND"},
		{name: "trailing slash", method: http.MethodGet, path: snapshotPath + "/?cursor=" + cursor, bearer: "fixture-access", status: 404, errorCode: "ROUTE_NOT_FOUND"},
		{name: "create first page", method: http.MethodPost, path: snapshotCollection, bearer: "fixture-access", body: `{"page_size":1}`, hasKey: true, status: 201, authCalls: 1, creates: 1},
		{name: "create missing Bearer before invalid body", method: http.MethodPost, path: snapshotCollection, body: "{", status: 401, errorCode: "AUTH_REQUIRED"},
		{name: "create expired Bearer before invalid body", method: http.MethodPost, path: snapshotCollection, bearer: "expired-fixture-access", body: "{", status: 401, authCalls: 1, errorCode: "TOKEN_EXPIRED"},
		{name: "create missing key after authorization", method: http.MethodPost, path: snapshotCollection, bearer: "fixture-access", body: "{}", status: 400, authCalls: 1, errorCode: "INVALID_ARGUMENT", field: "Idempotency-Key"},
		{name: "create invalid page size", method: http.MethodPost, path: snapshotCollection, bearer: "fixture-access", body: `{"page_size":0}`, hasKey: true, status: 400, authCalls: 1, errorCode: "INVALID_ARGUMENT", field: "page_size"},
		{name: "create trailing slash", method: http.MethodPost, path: snapshotCollection + "/", bearer: "fixture-access", body: "{}", hasKey: true, status: 404, errorCode: "ROUTE_NOT_FOUND"},
		{name: "collection wrong method", method: http.MethodPatch, path: snapshotCollection, bearer: "fixture-access", status: 404, errorCode: "ROUTE_NOT_FOUND"},
		{name: "confirm baseline without creation key", method: http.MethodPost, path: confirmPath, bearer: "fixture-access", body: `{"baseline_token":"` + baseline + `"}`, status: 200, authCalls: 1, confirms: 1},
		{name: "decoded confirm path and body", method: http.MethodPost, path: snapshotCollection + "/%30" + snapshotID[1:] + "/confirm", bearer: "fixture-access", body: `{"baseline_token":"baseline-payload\u002ebaseline-signature"}`, status: 200, authCalls: 1, confirms: 1},
		{name: "confirm missing Bearer before invalid path and body", method: http.MethodPost, path: snapshotCollection + "/bad-id/confirm", body: "{", status: 401, errorCode: "AUTH_REQUIRED"},
		{name: "confirm expired Bearer before invalid body", method: http.MethodPost, path: confirmPath, bearer: "expired-fixture-access", body: "{", status: 401, authCalls: 1, errorCode: "TOKEN_EXPIRED"},
		{name: "confirm invalid path after authorization", method: http.MethodPost, path: snapshotCollection + "/bad-id/confirm", bearer: "fixture-access", body: "{}", status: 400, authCalls: 1, errorCode: "INVALID_ARGUMENT", field: "snapshot_id"},
		{name: "confirm missing token after authorization", method: http.MethodPost, path: confirmPath, bearer: "fixture-access", body: "{}", status: 400, authCalls: 1, errorCode: "INVALID_CURSOR", field: "baseline_token"},
		{name: "confirm wrong method", method: http.MethodGet, path: confirmPath + "?cursor=" + cursor, bearer: "fixture-access", status: 404, errorCode: "ROUTE_NOT_FOUND"},
		{name: "confirm trailing slash", method: http.MethodPost, path: confirmPath + "/", bearer: "fixture-access", body: "{}", status: 404, errorCode: "ROUTE_NOT_FOUND"},
		{name: "confirm missing snapshot id", method: http.MethodPost, path: snapshotCollection + "/confirm", bearer: "fixture-access", body: "{}", status: 404, errorCode: "ROUTE_NOT_FOUND"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			authorizationCalls, pageCalls, createCalls, confirmCalls = 0, 0, 0, 0
			request := httptest.NewRequest(testCase.method, testCase.path, strings.NewReader(testCase.body))
			if testCase.body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			if testCase.hasKey {
				request.Header.Set("Idempotency-Key", " "+key+"\t")
			}
			if testCase.bearer != "" {
				request.Header.Set("Authorization", "Bearer "+testCase.bearer)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || authorizationCalls != testCase.authCalls ||
				pageCalls != testCase.reads || createCalls != testCase.creates || confirmCalls != testCase.confirms {
				t.Fatalf("响应状态=%d，授权调用=%d，续页调用=%d，创建调用=%d，确认调用=%d。响应=%s",
					response.Code, authorizationCalls, pageCalls, createCalls, confirmCalls, response.Body.String())
			}
			requestID := response.Header().Get("X-Request-ID")
			if requestID == "" {
				t.Fatal("响应缺少请求 ID")
			}
			if testCase.errorCode != "" {
				var body dto.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
					body.RequestID != requestID || body.Error.Code != testCase.errorCode ||
					(testCase.field != "" && body.Error.Details["field"] != testCase.field) {
					t.Fatalf("错误响应不符合契约：%s", response.Body.String())
				}
				return
			}
			if testCase.confirms != 0 {
				var body dto.ConfirmSyncSnapshotBaselineResponse
				if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.RequestID != requestID ||
					body.Data.Status != "ACTIVE" || body.Data.AckSequence != "17" || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("确认响应不符合契约：%s", response.Body.String())
				}
				return
			}
			var body dto.SyncSnapshotPageResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
				body.RequestID != requestID || body.Data.SnapshotID != snapshotID ||
				body.Data.NextCursor == nil || *body.Data.NextCursor != *page.NextCursor ||
				response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("成功响应不符合契约：%s", response.Body.String())
			}
		})
	}
}

func TestRegisterConfirmSyncSnapshotBaselineRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "confirmation"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("缺少依赖仍完成基线确认路由注册")
				}
			}()
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			confirm := func(context.Context, domain.Actor, uuid.UUID, string) (uint64, error) { return 0, nil }
			switch missing {
			case "router":
				router = nil
			case "authorization":
				authorize = nil
			case "confirmation":
				confirm = nil
			}
			RegisterConfirmSyncSnapshotBaselineRoute(router, authorize, confirm)
		})
	}
}

func TestRegisterCreateSyncSnapshotRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "creation"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("缺少依赖仍完成快照创建路由注册")
				}
			}()
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			create := func(context.Context, domain.Actor, string, int) (*domain.SyncSnapshotPageResult, error) {
				return nil, nil
			}
			switch missing {
			case "router":
				router = nil
			case "authorization":
				authorize = nil
			case "creation":
				create = nil
			}
			RegisterCreateSyncSnapshotRoute(router, authorize, create)
		})
	}
}

func TestRegisterSyncSnapshotPageRouteDependencies(t *testing.T) {
	for _, missing := range []string{"router", "authorization", "page reader"} {
		t.Run(missing, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("缺少依赖仍完成快照续页路由注册")
				}
			}()
			var router *gin.Engine
			if missing != "router" {
				router = NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			}
			authorize := func(context.Context, string) (*domain.Actor, error) { return nil, nil }
			readPage := func(context.Context, domain.Actor, uuid.UUID, string) (*domain.SyncSnapshotPageResult, error) {
				return nil, nil
			}
			if missing == "authorization" {
				authorize = nil
			}
			if missing == "page reader" {
				readPage = nil
			}
			RegisterSyncSnapshotPageRoute(router, authorize, readPage)
		})
	}
}
