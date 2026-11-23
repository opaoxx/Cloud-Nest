package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/binding"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/requestcontext"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

type baselineConfirmationHTTPTransaction struct {
	state   domain.SyncDeviceState
	updates int
}

func (transaction *baselineConfirmationHTTPTransaction) FindCurrentSyncDeviceState(context.Context) (domain.SyncDeviceState, error) {
	return transaction.state, nil
}

func (*baselineConfirmationHTTPTransaction) ValidateSyncSnapshotSequence(_ context.Context, sequence uint64) error {
	if sequence > 22 {
		return domain.ErrInvalidSyncSnapshotSequence
	}
	return nil
}

func (transaction *baselineConfirmationHTTPTransaction) ActivateSyncDeviceBaseline(_ context.Context, generation domain.SyncCursorGeneration, cutoff uint64) (bool, error) {
	if transaction.state.Status != domain.SyncDeviceStatusResetRequired || transaction.state.Generation != generation {
		return false, nil
	}
	transaction.state.Status = domain.SyncDeviceStatusActive
	transaction.state.AckSequence = cutoff
	transaction.updates++
	return true, nil
}

func TestConfirmSyncSnapshotBaselineHandlerFlow(t *testing.T) {
	// 1. 用真实末页签名、合成身份及事务替身构造独立测试路由。
	const pathPrefix = "/test-sync-snapshots/"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const diagnostic = "fixture-internal-confirmation-diagnostic"
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 91}
	snapshotBinding := service.SyncSnapshotCacheBinding{
		SnapshotID: uuid.MustParse("052dc4f7-f4c1-46b3-a509-bf96992f6580"), UserID: actor.UserID, DeviceID: actor.DeviceID,
		Generation: domain.SyncCursorGeneration{0xf1, 0, 3, 4, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 0xfe},
	}
	masterKey := bytes.Repeat([]byte{0x61}, sha256.Size)
	signer, err := service.NewSyncSnapshotBaselineSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewSyncSnapshotBaselineVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Date(2026, 11, 23, 2, 10, 0, 0, time.UTC)
	token, err := signer.Sign(snapshotBinding, 17, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	makeBody := func(value string) string { return `{"baseline_token":"` + value + `"}` }
	for _, testCase := range []struct {
		name, path, bearer, body, mediaType string
		status                              int
		code, field, sequence               string
		calls, transactions, updates        int
		expired, stale, active, canceled    bool
		transactionError                    error
	}{
		{name: "confirm with escaped token", body: makeBody(strings.Replace(token, ".", `\u002e`, 1)), status: 200, sequence: "17", calls: 1, transactions: 1, updates: 1},
		{name: "retry keeps advanced position", status: 200, sequence: "20", calls: 1, transactions: 1, active: true},
		{name: "authentication precedes parser", bearer: "missing", path: "bad-id", body: "!", status: 401, code: "AUTH_REQUIRED"},
		{name: "disabled before parsing", bearer: "disabled", body: "!", status: 403, code: "ACCOUNT_DISABLED"},
		{name: "invalid snapshot id", path: "bad-id", status: 400, code: "INVALID_ARGUMENT", field: "snapshot_id"},
		{name: "missing token", body: "{}", status: 400, code: "INVALID_CURSOR", field: "baseline_token"},
		{name: "duplicate token", body: `{"baseline_token":"a.b","baseline_token":"c.d"}`, status: 400, code: "INVALID_ARGUMENT"},
		{name: "large body", body: "{}" + strings.Repeat(" ", int(binding.MaxJSONBodyBytes)), status: 413, code: "REQUEST_BODY_TOO_LARGE"},
		{name: "unsupported body", mediaType: "text/plain", status: 415, code: "UNSUPPORTED_MEDIA_TYPE"},
		{name: "wrong device", bearer: "other-device", status: 400, code: "INVALID_CURSOR", field: "baseline_token", calls: 1},
		{name: "tampered token", body: makeBody(token + "A"), status: 400, code: "INVALID_CURSOR", field: "baseline_token", calls: 1},
		{name: "token expired", expired: true, status: 410, code: "CURSOR_EXPIRED", calls: 1},
		{name: "stale generation", stale: true, status: 410, code: "CURSOR_EXPIRED", calls: 1, transactions: 1},
		{name: "session revoked in transaction", transactionError: domain.ErrSessionRevoked, status: 401, code: "SESSION_REVOKED", calls: 1, transactions: 1},
		{name: "account disabled in transaction", transactionError: domain.ErrAccountDisabled, status: 403, code: "ACCOUNT_DISABLED", calls: 1, transactions: 1},
		{name: "commit unconfirmed", transactionError: domain.ErrUserTransactionCommitUnconfirmed, status: 500, code: "INTERNAL_ERROR", calls: 1, transactions: 1},
		{name: "canceled during service", canceled: true, status: 500, code: "INTERNAL_ERROR", calls: 1, transactions: 1, updates: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fixture := &baselineConfirmationHTTPTransaction{
				state: domain.SyncDeviceState{Status: domain.SyncDeviceStatusResetRequired, Generation: snapshotBinding.Generation},
			}
			if testCase.active {
				fixture.state.Status, fixture.state.AckSequence = domain.SyncDeviceStatusActive, 20
			}
			if testCase.stale {
				fixture.state.Generation[15] ^= 1
			}
			stamp := expiresAt.Add(-time.Nanosecond)
			if testCase.expired {
				stamp = expiresAt
			}
			calls, transactions := 0, 0
			confirmer := service.NewSyncSnapshotBaselineConfirmationService(verifier,
				func(txContext context.Context, identity domain.Actor, action func(context.Context, service.SyncSnapshotBaselineConfirmationTransaction) error) error {
					transactions++
					if identity != actor {
						t.Fatal("传入账号事务的认证身份不符")
					}
					if testCase.transactionError != nil {
						return fmt.Errorf("%s: %w", diagnostic, testCase.transactionError)
					}
					return action(txContext, fixture)
				}, func() time.Time { return stamp })
			var logs bytes.Buffer
			router := NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
			authorize := middleware.RequireAuthorization(func(_ context.Context, bearer string) (*domain.Actor, error) {
				identity := actor
				switch bearer {
				case "other-device":
					identity.DeviceID++
				case "disabled":
					return nil, domain.ErrAccountDisabled
				}
				return &identity, nil
			}, handler.AuthorizationFailure)
			router.POST(pathPrefix+":snapshot_id/confirm", authorize, handler.ConfirmSyncSnapshotBaseline(func(callContext context.Context, identity domain.Actor, snapshotID uuid.UUID, rawToken string) (uint64, error) {
				calls++
				if snapshotID != snapshotBinding.SnapshotID || requestcontext.RequestID(callContext) != requestID ||
					(testCase.status == 200 && rawToken != token) {
					t.Fatal("确认服务收到的路径、请求 ID 或凭据原文不符")
				}
				position, err := confirmer.Confirm(callContext, identity, snapshotID, rawToken)
				if testCase.canceled {
					cancel()
				}
				return position, err
			}), func(*gin.Context) { t.Error("响应已结束后进入下游") })

			// 2. 发送请求并核对调用次数、外层状态和公开字段。
			path, body, bearer, mediaType := testCase.path, testCase.body, testCase.bearer, testCase.mediaType
			if path == "" {
				path = snapshotBinding.SnapshotID.String()
			}
			if body == "" {
				body = makeBody(token)
			}
			if bearer == "" {
				bearer = "owner"
			}
			if mediaType == "" {
				mediaType = "application/json"
			}
			request := httptest.NewRequest(http.MethodPost, pathPrefix+path+"/confirm", strings.NewReader(body)).WithContext(ctx)
			request.Header.Set("Content-Type", mediaType)
			request.Header.Set("X-Request-ID", requestID)
			if bearer != "missing" {
				request.Header.Set("Authorization", "Bearer "+bearer)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != testCase.status || calls != testCase.calls || transactions != testCase.transactions || fixture.updates != testCase.updates ||
				response.Header().Get("X-Request-ID") != requestID || response.Header().Get("Cache-Control") != "no-store" ||
				strings.Contains(response.Body.String(), token) || strings.Contains(logs.String(), token) || strings.Contains(response.Body.String(), diagnostic) {
				t.Fatalf("状态=%d，确认=%d，事务=%d，更新=%d，正文=%s", response.Code, calls, transactions, fixture.updates, response.Body.String())
			}
			if testCase.status == http.StatusOK {
				var confirmed dto.ConfirmSyncSnapshotBaselineResponse
				if json.Unmarshal(response.Body.Bytes(), &confirmed) != nil || confirmed.RequestID != requestID ||
					confirmed.Data.Status != "ACTIVE" || confirmed.Data.AckSequence != testCase.sequence {
					t.Fatalf("确认成功响应不符：%s", response.Body.String())
				}
				return
			}
			var failure dto.ErrorResponse
			if json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.RequestID != requestID || failure.Error.Code != testCase.code ||
				(testCase.field != "" && failure.Error.Details["field"] != testCase.field) || (testCase.field == "" && failure.Error.Details != nil) ||
				(testCase.status == http.StatusUnauthorized && response.Header().Get("WWW-Authenticate") != `Bearer realm="cloudnest"`) {
				t.Fatalf("确认错误响应不符：%s", response.Body.String())
			}
		})
	}
}
