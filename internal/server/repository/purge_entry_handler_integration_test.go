package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestPurgeEntryHandlerIntegration(t *testing.T) {
	// 1. 在隔离库准备真实会话和回收目录，签发测试访问令牌。
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pool, store, actor := trashEntryIdempotencyTestDatabase(t, ctx)
	installChangeEventFixtureTable(t, ctx, pool)
	jwtConfig := config.JWTConfig{
		SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test",
		AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: 24 * time.Hour,
	}
	tokenSigner, err := service.NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	tokenVerifier, err := service.NewAccessTokenVerifier(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokenSigner.Issue(domain.LoginSession{
		ID: actor.SessionID, UserID: actor.UserID, DeviceID: actor.DeviceID, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	previewSigner, err := service.NewPurgePreviewSigner(jwtConfig.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	previewVerifier, err := service.NewPurgePreviewVerifier(jwtConfig.SigningKey)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 将预检快照和用户事务绑定到正式 HTTP 路由。
	now := time.Now().UTC().Truncate(time.Microsecond)
	currentTime := func() time.Time { return now }
	preview := service.NewPurgePreviewService(func(ctx context.Context, action func(context.Context, service.PurgePreviewReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(readContext context.Context, readStore *Store) error {
			return action(readContext, readStore)
		})
	}, previewSigner, currentTime)
	purge := service.NewIdempotentPurgeEntryService(
		func(ctx context.Context, identity domain.Actor, action func(context.Context, service.IdempotentPurgeEntryTransaction) error) error {
			return store.WithUserTransaction(ctx, identity, func(txContext context.Context, txStore *Store) error {
				return action(txContext, txStore)
			})
		}, previewVerifier,
		func(entry *domain.FileEntry) (json.RawMessage, error) {
			data, err := dto.NewPurgeEntryAcceptedData(entry)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		},
		func(body []byte) (string, error) {
			data, err := dto.DecodePurgeEntryAcceptedData(body)
			if err != nil {
				return "", err
			}
			return data.EntryID, nil
		}, currentTime,
	)
	authorization := service.NewAuthorizationService(tokenVerifier.Verify, store.FindSessionAuthorization)
	router := serverhttp.NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	serverhttp.RegisterPurgePreviewRoute(router, authorization.Authorize, preview.Preview)
	serverhttp.RegisterPurgeEntryRoute(router, authorization.Authorize, purge.Begin)

	// 3. 取得同一预检后验证失败、首次确认及凭证到期后的同键重试。
	const path = "/api/v1/trash/14"
	previewRequest := httptest.NewRequest(http.MethodGet, path+"/purge-preview", nil).WithContext(ctx)
	previewRequest.Header.Set("Authorization", "Bearer "+token.Token)
	previewResponse := httptest.NewRecorder()
	router.ServeHTTP(previewResponse, previewRequest)
	var preflight dto.PurgePreviewResponse
	if err := json.Unmarshal(previewResponse.Body.Bytes(), &preflight); err != nil ||
		previewResponse.Code != http.StatusOK || preflight.Data.EntryID != "14" || preflight.Data.Revision != "1" {
		t.Fatal("未通过 HTTP 取得清理凭证", previewResponse.Code, err)
	}

	const key = "purge-http-14"
	send := func(revision string, expectedStatus int) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodDelete, path, nil).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+token.Token)
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set(dto.PurgeExpectedRevisionHeader, revision)
		request.Header.Set(dto.PurgePreviewDigestHeader, preflight.Data.PreviewDigest)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != expectedStatus || response.Header().Get("X-Request-ID") == "" ||
			response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Pragma") != "no-cache" {
			t.Fatal("清理确认状态或响应头不符", response.Code, response.Header())
		}
		return response
	}
	readAccepted := func(response *httptest.ResponseRecorder) dto.PurgeEntryAcceptedResponse {
		t.Helper()
		var accepted dto.PurgeEntryAcceptedResponse
		err := json.Unmarshal(response.Body.Bytes(), &accepted)
		want := dto.PurgeEntryAcceptedData{EntryID: "14", Status: "PURGING", ProgressURL: "/api/v1/trash/14/purge"}
		if err != nil || accepted.Data != want || accepted.RequestID != response.Header().Get("X-Request-ID") {
			t.Fatal("HTTP 受理快照或请求 ID 不符", err)
		}
		return accepted
	}
	readError := func(response *httptest.ResponseRecorder, want string) {
		t.Helper()
		var failure dto.ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil || failure.Error.Code != want {
			t.Fatal("清理确认错误码不符", want, failure.Error.Code, err)
		}
	}
	checkLog := func(want uint64) {
		t.Helper()
		var sequence, events, snapshots uint64
		if err := pool.QueryRowContext(ctx, `SELECT change_seq,(SELECT COUNT(*) FROM change_events),
			(SELECT COUNT(*) FROM idempotency_records) FROM users WHERE id=?`, actor.UserID).Scan(&sequence, &events, &snapshots); err != nil ||
			sequence != want || events != want || snapshots != want {
			t.Fatal("清理受理快照、事件数或账号序号不符", snapshots, events, sequence, err)
		}
	}
	readError(send("2", http.StatusConflict), "REVISION_CONFLICT")
	validTime := now
	now = preflight.Data.ExpiresAt.Add(time.Second)
	readError(send(preflight.Data.Revision, http.StatusConflict), "PURGE_PREVIEW_CHANGED")
	checkLog(0)
	now = validTime

	first := readAccepted(send(preflight.Data.Revision, http.StatusAccepted))
	now = preflight.Data.ExpiresAt.Add(time.Second)
	replay := readAccepted(send(preflight.Data.Revision, http.StatusAccepted))
	if replay.Data != first.Data || replay.RequestID == first.RequestID {
		t.Fatal("过期凭证的同键同参重试应复用受理快照并分配新请求 ID")
	}
	conflict := send("2", http.StatusConflict)
	var failure dto.ErrorResponse
	if err := json.Unmarshal(conflict.Body.Bytes(), &failure); err != nil ||
		failure.Error.Code != "IDEMPOTENCY_CONFLICT" || failure.Error.Details != nil {
		t.Fatal("同键异参应返回幂等冲突", err)
	}

	// 4. 读回目录项与幂等记录，核对受理状态和版本只写入一次。
	entry, err := store.FindUserFileEntry(ctx, actor.UserID, 14)
	if err != nil || entry == nil || entry.Status != "PURGING" || entry.Revision != 2 {
		t.Fatal("首次受理后的目录项状态或版本错误", entry, err)
	}
	record, err := store.FindUserIdempotencyRecord(ctx, actor.UserID, domain.PurgeEntryOperation, key)
	if err != nil || record == nil || record.ResponseStatus != http.StatusAccepted {
		t.Fatal("首次 202 快照未保存", err)
	}
	saved, err := dto.DecodePurgeEntryAcceptedData(record.ResponseBody)
	if err != nil || *saved != first.Data {
		t.Fatal("已保存快照与 HTTP data 不符", err)
	}
	checkLog(1)
	var savedID, savedRevision uint64
	var eventType string
	var rawPayload []byte
	if err := pool.QueryRowContext(ctx, "SELECT entry_id,revision,event_type,payload FROM change_events WHERE user_id=? AND `sequence`=1",
		actor.UserID).Scan(&savedID, &savedRevision, &eventType, &rawPayload); err != nil {
		t.Fatal("未读到正式 HTTP 提交的清理受理事件", err)
	}
	want := map[string]string{"entry_id": "14", "parent_id": "11", "name": "已回收目录", "revision": "2"}
	var payload map[string]string
	if err := json.Unmarshal(rawPayload, &payload); err != nil || !reflect.DeepEqual(payload, want) ||
		eventType != "ENTRY_PURGE_STARTED" || savedID != entry.ID || savedRevision != entry.Revision {
		t.Fatal("清理受理事件与根项不符", string(rawPayload), err)
	}
	var retainedChild bool
	if err := pool.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM file_entries WHERE id=15 AND parent_id=14 AND user_id=?
		AND status='ACTIVE' AND revision=5 AND object_id=41 AND purged_at IS NULL)`, actor.UserID).Scan(&retainedChild); err != nil || !retainedChild {
		t.Fatal("受理时改变了后代文件的状态、版本或对象引用", err)
	}
	t.Log("HTTP 版本冲突及预检到期时事件为 0，首次 202 仅为目录 14 写入序号 1，同键重试无新增事件")
}
