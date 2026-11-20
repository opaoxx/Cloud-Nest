package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestTrashPageHandlerIntegration(t *testing.T) {
	// 1. 复用独占库，准备同一时刻的两项、隐藏子项及另一账号的回收项。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, store, owner := trashEntryTestDatabase(t, ctx)
	stamp := time.Date(2026, 11, 20, 4, 30, 0, 123456000, time.UTC)
	execute := func(statement string, arguments ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, arguments...); err != nil {
			t.Fatal("建立回收站列表夹具失败", err)
		}
	}
	execute("UPDATE file_entries SET status='TRASHED', deleted_at=? WHERE user_id=1 AND id IN (13,14)", stamp)
	execute("UPDATE file_entries SET status='TRASHED', deleted_at=? WHERE user_id=1 AND id=15", stamp.Add(time.Second))
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
		VALUES (22,2,21,'其他回收项',CAST('其他回收项' AS BINARY),'DIR','TRASHED',?)`, stamp)
	execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform)
		VALUES (29,2,'28ee758a-0727-42de-ae7d-3bb33155e149','另一账号设备','WEB')`)
	execute(`INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at)
		VALUES (27,2,29,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`)

	// 2. 用真实会话签发访问令牌，接入共享认证与正式分页服务。
	jwtConfig := config.JWTConfig{
		SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test",
		AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour,
	}
	accessSigner, err := service.NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	accessVerifier, err := service.NewAccessTokenVerifier(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(actor domain.Actor) string {
		t.Helper()
		session := domain.LoginSession{ID: actor.SessionID, UserID: actor.UserID, DeviceID: actor.DeviceID}
		if err := pool.QueryRowContext(ctx, "SELECT expires_at FROM auth_sessions WHERE user_id=? AND id=?", actor.UserID, actor.SessionID).
			Scan(&session.ExpiresAt); err != nil {
			t.Fatal(err)
		}
		token, err := accessSigner.Issue(session)
		if err != nil {
			t.Fatal(err)
		}
		return token.Token
	}
	ownerToken := issue(owner)
	otherToken := issue(domain.Actor{UserID: 2, SessionID: 27, DeviceID: 29})
	authorization := service.NewAuthorizationService(accessVerifier.Verify, store.FindSessionAuthorization)
	snapshotCalls := 0
	list := service.NewTrashEntryListService(func(current context.Context, read func(context.Context, service.TrashEntryListReader) error) error {
		snapshotCalls++
		return store.WithFileEntrySnapshot(current, func(readContext context.Context, reader *Store) error {
			return read(readContext, reader)
		})
	})
	signer, err := service.NewTrashEntryCursorSigner(jwtConfig.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewTrashEntryCursorVerifier(jwtConfig.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	pages := service.NewTrashEntryPageService(verifier.Verify, list.List, signer.Sign)
	router := serverhttp.NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	const path = "/test-trash-page"
	const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	router.GET(path, middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure), handler.TrashPage(pages.List))

	// 3. 从 HTTP 请求核对出错阶段、账号隔离和分页结果。
	request := func(token, rawQuery string, status int, code, field string) dto.TrashPageData {
		t.Helper()
		message := httptest.NewRequest(http.MethodGet, path+"?"+rawQuery, nil).WithContext(ctx)
		message.Header.Set("X-Request-ID", requestID)
		if token != "" {
			message.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, message)
		if response.Code != status || response.Header().Get("X-Request-ID") != requestID ||
			response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("HTTP 状态或头不符，状态为 %d，正文为 %s", response.Code, response.Body.String())
		}
		if status == http.StatusOK {
			var body dto.TrashPageResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.RequestID != requestID || body.Data.Items == nil {
				t.Fatalf("分页结果为 %s，错误为 %v", response.Body.String(), err)
			}
			return body.Data
		}
		var body dto.ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.RequestID != requestID ||
			body.Error.Code != code || (field != "" && body.Error.Details["field"] != field) {
			t.Fatalf("HTTP 错误为 %s，解码错误为 %v", response.Body.String(), err)
		}
		return dto.TrashPageData{}
	}
	ids := func(data dto.TrashPageData) []string {
		result := make([]string, 0, len(data.Items))
		for _, item := range data.Items {
			result = append(result, item.ID)
		}
		return result
	}
	request("", "limit=0", 401, "AUTH_REQUIRED", "")
	request(ownerToken, "limit=0", 400, "INVALID_ARGUMENT", "limit")
	request(ownerToken, "cursor=invalid", 400, "INVALID_CURSOR", "cursor")
	if snapshotCalls != 0 {
		t.Fatal("无效查询打开了列表快照")
	}
	first := request(ownerToken, "limit=1&user_id=2", 200, "", "")
	if !slices.Equal(ids(first), []string{"14"}) || first.NextCursor == nil {
		t.Fatalf("所有者首页为 %+v", first)
	}
	query := url.Values{"limit": {"2"}, "cursor": {*first.NextCursor}}.Encode()
	request(otherToken, query, 400, "INVALID_CURSOR", "cursor")
	if snapshotCalls != 1 {
		t.Fatal("其他账号的游标打开了列表快照")
	}
	last := request(ownerToken, query, 200, "", "")
	if !slices.Equal(ids(last), []string{"13"}) || last.NextCursor != nil {
		t.Fatalf("所有者末页为 %+v", last)
	}
	item := last.Items[0]
	if len(item.OriginalPath) != 2 || item.OriginalPath[0].ID != "11" || item.OriginalPath[1].ID != "12" ||
		item.SizeBytes != 2048 || !item.RetentionExpiresAt.Equal(stamp.AddDate(0, 0, 30)) {
		t.Fatalf("回收文件公开字段为 %+v", item)
	}
	other := request(otherToken, "user_id=1", 200, "", "")
	if !slices.Equal(ids(other), []string{"22"}) || other.NextCursor != nil || snapshotCalls != 3 {
		t.Fatalf("另一账号列表为 %+v，快照次数为 %d", other, snapshotCalls)
	}
	execute("UPDATE users SET status='DISABLED' WHERE id=2")
	request(otherToken, "limit=0", 403, "ACCOUNT_DISABLED", "")
	if snapshotCalls != 3 {
		t.Fatal("禁用账号打开了列表快照")
	}
}
