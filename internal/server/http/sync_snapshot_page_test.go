package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

const syncSnapshotPageTestPath = "/test-sync-snapshots/"

func TestSyncSnapshotPageHandlerFlow(t *testing.T) {
	// 1. 用内存快照和合成认证装配完整的测试请求链，复用正式签发器与读取服务。
	stamp := time.Date(2026, 11, 23, 2, 0, 0, 123456789, time.UTC)
	now := stamp
	clock := func() time.Time { return now }
	binding := service.SyncSnapshotCacheBinding{
		SnapshotID: syncSnapshotPageSuccessFixture().SnapshotID, UserID: 7, DeviceID: 19, PageSize: 2,
		Generation: domain.SyncCursorGeneration{0xf1, 2, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe},
	}
	cache := service.NewSyncSnapshotCache(clock)
	var entries []domain.SyncSnapshotEntry
	for index := range 5 {
		entries = append(entries, domain.SyncSnapshotEntry{ID: uint64(12 + index), ParentID: 11, Name: "资料" + strconv.Itoa(index), EntryType: "DIR", Revision: 1})
	}
	encoded, err := dto.EncodeSyncSnapshotEntries(entries)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := cache.ReserveBuild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	expiresAt, err := cache.Put(t.Context(), reservation, binding, &service.BuiltSyncSnapshot{
		Header: domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: 42}, Entries: *encoded,
	})
	if err != nil {
		t.Fatal(err)
	}
	master := bytes.Repeat([]byte{0x91}, sha256.Size)
	verifier, err := service.NewSyncSnapshotCursorVerifier(master)
	if err != nil {
		t.Fatal(err)
	}
	cursorSigner, err := service.NewSyncSnapshotCursorSigner(master)
	if err != nil {
		t.Fatal(err)
	}
	baselineSigner, err := service.NewSyncSnapshotBaselineSigner(master)
	if err != nil {
		t.Fatal(err)
	}
	delivery := service.NewSyncSnapshotPageDelivery(cursorSigner.Sign, baselineSigner.Sign)
	credential, err := cursorSigner.Sign(binding, 2, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	lastCredential, err := cursorSigner.Sign(binding, 4, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	generation := binding.Generation
	readCalls, generationCalls := 0, 0
	reader := service.NewSyncSnapshotContinuationService(verifier.Verify, func(_ context.Context, userID, deviceID uint64) (domain.SyncCursorGeneration, error) {
		generationCalls++
		if userID != binding.UserID || deviceID != binding.DeviceID {
			t.Fatal("查询代次使用了错误身份")
		}
		return generation, nil
	}, cache.Get, clock)
	var logs bytes.Buffer
	router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
	authorize := middleware.RequireAuthorization(func(_ context.Context, token string) (*domain.Actor, error) {
		actor := &domain.Actor{UserID: binding.UserID, DeviceID: binding.DeviceID, SessionID: 91}
		switch token {
		case "owner":
		case "other-device":
			actor.DeviceID++
		case "disabled":
			return nil, domain.ErrAccountDisabled
		default:
			return nil, domain.ErrInvalidAccessToken
		}
		return actor, nil
	}, handler.AuthorizationFailure)
	router.GET(syncSnapshotPageTestPath+":snapshot_id", authorize, handler.SyncSnapshotPage(func(ctx context.Context, actor domain.Actor, id uuid.UUID, cursor string) (*domain.SyncSnapshotPageResult, error) {
		readCalls++
		page, err := reader.Read(ctx, actor, id, cursor)
		if err != nil {
			return nil, err
		}
		return delivery.Build(ctx, page.Binding, page.Header, page.ExpiresAt, &page.Page)
	}), func(*gin.Context) { t.Error("续页处理结束后进入下游") })

	// 2. 核对完整 HTTP 路径、认证先行和错误映射。
	for _, testCase := range []struct {
		name, auth, path, query, code, field string
		status, wantRead, wantGeneration     int
	}{
		{name: "middle", auth: "owner", query: "cursor=" + credential, status: 200, wantRead: 1, wantGeneration: 1},
		{name: "middle replay", auth: "owner", query: "cursor=" + strings.Replace(credential, ".", "%2E", 1), status: 200, wantRead: 1, wantGeneration: 1},
		{name: "last", auth: "owner", query: "cursor=" + lastCredential, status: 200, wantRead: 1, wantGeneration: 1},
		{name: "authentication precedes path", path: "bad-id", query: "", status: 401, code: "AUTH_REQUIRED"},
		{name: "disabled account", auth: "disabled", status: 403, code: "ACCOUNT_DISABLED"},
		{name: "bad path", auth: "owner", path: strings.ToUpper(binding.SnapshotID.String()), query: "cursor=" + credential, status: 400, code: "INVALID_ARGUMENT", field: "snapshot_id"},
		{name: "malformed query", auth: "owner", query: "cursor=%ZZ", status: 400, code: "INVALID_ARGUMENT", field: "query"},
		{name: "missing cursor", auth: "owner", status: 400, code: "INVALID_CURSOR", field: "cursor"},
		{name: "duplicate cursor", auth: "owner", query: "cursor=" + credential + "&cursor=" + credential, status: 400, code: "INVALID_CURSOR", field: "cursor"},
		{name: "tampered cursor", auth: "owner", query: "cursor=A" + credential[1:], status: 400, code: "INVALID_CURSOR", field: "cursor", wantRead: 1},
		{name: "wrong device", auth: "other-device", query: "cursor=" + credential, status: 400, code: "INVALID_CURSOR", field: "cursor", wantRead: 1},
		{name: "stale generation", auth: "owner", query: "cursor=" + credential, status: 410, code: "CURSOR_EXPIRED", wantRead: 1, wantGeneration: 1},
		{name: "expired cursor", auth: "owner", query: "cursor=" + credential, status: 410, code: "CURSOR_EXPIRED", wantRead: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			now, generation = stamp, binding.Generation
			switch testCase.name {
			case "stale generation":
				generation[15] ^= 1
			case "expired cursor":
				now = expiresAt
			}
			path := testCase.path
			if path == "" {
				path = binding.SnapshotID.String()
			}
			request := httptest.NewRequest(http.MethodGet, syncSnapshotPageTestPath+path, nil)
			request.URL.RawQuery = testCase.query
			if testCase.auth != "" {
				request.Header.Set("Authorization", "Bearer "+testCase.auth)
			}
			response := httptest.NewRecorder()
			priorRead, priorGeneration := readCalls, generationCalls
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || response.Header().Get("Cache-Control") != "no-store" ||
				readCalls-priorRead != testCase.wantRead || generationCalls-priorGeneration != testCase.wantGeneration {
				t.Fatalf("续页响应或调用顺序不符，状态=%d，读取=%d，查代次=%d", response.Code, readCalls-priorRead, generationCalls-priorGeneration)
			}
			requestID := response.Header().Get("X-Request-ID")
			if testCase.status == 200 {
				var body dto.SyncSnapshotPageResponse
				if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.RequestID == "" || body.RequestID != requestID ||
					body.Data.SnapshotID != binding.SnapshotID.String() || body.Data.HighWaterSequence != "42" || !body.Data.ExpiresAt.Equal(expiresAt) {
					t.Fatal("成功页面未保留原快照信息和请求 ID")
				}
				if testCase.name == "last" {
					if len(body.Data.Items) != 1 || body.Data.NextCursor != nil || body.Data.BaselineToken == nil {
						t.Fatal("末页必须交付一个条目和确认凭据")
					}
				} else if len(body.Data.Items) != 2 || body.Data.NextCursor == nil || *body.Data.NextCursor != lastCredential || body.Data.BaselineToken != nil {
					t.Fatal("中间页应交付相同的下一位置凭据")
				}
				return
			}
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || body.RequestID == "" || body.RequestID != requestID || body.Error.Code != testCase.code {
				t.Fatalf("续页错误契约不符，实际=%s", response.Body.String())
			}
			if testCase.field == "" {
				if body.Error.Details != nil {
					t.Fatal("此错误应使用空 details")
				}
			} else if body.Error.Details["field"] != testCase.field {
				t.Fatal("错误字段不符")
			}
		})
	}
	if strings.Contains(logs.String(), credential) || strings.Contains(logs.String(), lastCredential) {
		t.Fatal("请求完成日志包含合成凭据")
	}
}

func TestSyncSnapshotPageHandlerRejectsBadServiceResult(t *testing.T) {
	// 1. 使用测试桩注入异常结果，确认统一 500 不会泄露细节或返回部分页面。
	for _, scenario := range []string{"nil result", "wrong snapshot", "database error", "request canceled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := syncSnapshotPageSuccessFixture()
			path := syncSnapshotPageTestPath + result.SnapshotID.String()
			router := NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
			router.GET(syncSnapshotPageTestPath+":snapshot_id", middleware.RequireAuthorization(func(context.Context, string) (*domain.Actor, error) {
				return &domain.Actor{UserID: 7, DeviceID: 19, SessionID: 91}, nil
			}, handler.AuthorizationFailure), handler.SyncSnapshotPage(func(context.Context, domain.Actor, uuid.UUID, string) (*domain.SyncSnapshotPageResult, error) {
				switch scenario {
				case "nil result":
					return nil, nil
				case "wrong snapshot":
					result.SnapshotID[15] ^= 1
				case "database error":
					return nil, errors.New("private database failure")
				case "request canceled":
					cancel()
				}
				return result, nil
			}))
			request := httptest.NewRequest(http.MethodGet, path+"?cursor=AQ.Ag", nil).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer owner")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			var body dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != 500 || body.Error.Code != "INTERNAL_ERROR" || body.Error.Details != nil ||
				strings.Contains(response.Body.String(), "private database failure") || strings.Contains(response.Body.String(), "fixture-signature") {
				t.Fatal("异常结果必须走统一 500")
			}
		})
	}
}
