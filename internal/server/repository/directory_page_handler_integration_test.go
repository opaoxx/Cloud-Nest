package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/http/middleware"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestDirectoryPageHandlerIntegration(t *testing.T) {
	// 1. 复用正式元数据表独占库，追加正式会话表并建立目录夹具。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	if start < 0 {
		t.Fatal("未找到正式会话表")
	}
	end := strings.Index(migrations.InitialSQL[start:], "CREATE TABLE stored_objects (")
	if end < 0 {
		t.Fatal("未找到会话表结束边界")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:start+end]); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','目录处理用户','fixture_hash'),(3,'fixture_three','其他用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-directory-handler',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(14,1,11,'祖先目录',CAST('祖先目录' AS BINARY),'DIR'),(12,1,14,'当前目录',CAST('当前目录' AS BINARY),'DIR'),(15,1,11,'空目录',CAST('空目录' AS BINARY),'DIR'),(21,1,12,'子项一',CAST('子项一' AS BINARY),'DIR'),(22,1,12,'子项二',CAST('子项二' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(23,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048,3),(42,1,11,'根下文件.md',CAST('根下文件.md' AS BINARY),'FILE','DOCUMENT',41,2048,1)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=?", stamp); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	configuration := config.JWTConfig{SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test", AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour}
	tokenSigner, err := service.NewAccessTokenSigner(configuration)
	if err != nil {
		t.Fatal(err)
	}
	tokenVerifier, err := service.NewAccessTokenVerifier(configuration)
	if err != nil {
		t.Fatal(err)
	}
	authorization := service.NewAuthorizationService(tokenVerifier.Verify, store.FindSessionAuthorization)
	var secrets []string
	newLogin := func(t *testing.T, userID uint64) (string, domain.Actor) {
		t.Helper()
		_, hash := service.GenerateRefreshToken()
		var session *domain.LoginSession
		if err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, uuid.NewString(), "目录 HTTP 测试", "WEB")
			if err != nil {
				return err
			}
			session, err = txStore.CreateLoginSession(txContext, device.ID, hash, time.Now().Add(time.Hour))
			return err
		}); err != nil || session == nil {
			t.Fatal("创建认证夹具失败", err)
		}
		token, err := tokenSigner.Issue(*session)
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, token.Token)
		return token.Token, domain.Actor{UserID: userID, SessionID: session.ID, DeviceID: session.DeviceID}
	}
	withSnapshot := func(ctx context.Context, action func(context.Context, service.DirectoryListReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, txStore *Store) error { return action(snapshotContext, txStore) })
	}
	lists := service.NewDirectoryListService(withSnapshot)
	cursorSigner, err := service.NewDirectoryCursorSigner(configuration.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	cursorVerifier, err := service.NewDirectoryCursorVerifier(configuration.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	pages := service.NewDirectoryPageService(cursorVerifier.Verify, lists.List, cursorSigner.Sign)
	failure := errors.New("private-snapshot-failure")
	failingLists := service.NewDirectoryListService(func(ctx context.Context, action func(context.Context, service.DirectoryListReader) error) error {
		return withSnapshot(ctx, func(snapshotContext context.Context, reader service.DirectoryListReader) error {
			if err := action(snapshotContext, reader); err != nil {
				return err
			}
			return failure
		})
	})
	failingSnapshot := service.NewDirectoryPageService(cursorVerifier.Verify, failingLists.List, cursorSigner.Sign)
	failingSign := service.NewDirectoryPageService(cursorVerifier.Verify, lists.List, func(uint64, uint64, domain.FileEntryPosition) (string, error) { return "", failure })
	type modeKey struct{}
	var calls atomic.Int32
	var logs bytes.Buffer
	router := serverhttp.NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	router.GET("/api/v1/test-directory-pages", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure), func(requestContext *gin.Context) {
		requestContext.Request = requestContext.Request.WithContext(context.WithValue(requestContext.Request.Context(), modeKey{}, requestContext.GetHeader("X-Test-Mode")))
	}, handler.DirectoryPage(func(ctx context.Context, actor domain.Actor, parentID uint64, cursor string, size int) (*domain.DirectoryPageResult, error) {
		calls.Add(1)
		switch ctx.Value(modeKey{}) {
		case "snapshot-failure":
			return failingSnapshot.List(ctx, actor, parentID, cursor, size)
		case "sign-failure":
			return failingSign.List(ctx, actor, parentID, cursor, size)
		case "disable-after-auth":
			if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=?", actor.UserID); err != nil {
				return nil, err
			}
		case "panic":
			panic("private-service-panic")
		}
		return pages.List(ctx, actor, parentID, cursor, size)
	}))
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	request := func(t *testing.T, token, raw, mode string, status, serviceCalls int, code, field string) dto.DirectoryPageResponse {
		t.Helper()
		before := calls.Load()
		message, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v1/test-directory-pages?"+raw+"&user_id=99&note=private-query", nil)
		if err != nil {
			t.Fatal(err)
		}
		message.Header.Set("Origin", "http://127.0.0.1:5173")
		message.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
		message.Header.Set("X-Test-Mode", mode)
		message.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-cookie"})
		if token != "" {
			message.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(message)
		if err != nil {
			t.Fatal(err)
		}
		encoded, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if readError != nil || closeError != nil || response.StatusCode != status || int(calls.Load()-before) != serviceCalls || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Content-Encoding") != "" || response.Header.Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || len(response.Cookies()) != 0 {
			t.Fatal("网络状态、服务调用或响应头错误", response.StatusCode, calls.Load()-before, readError, closeError)
		}
		var result dto.DirectoryPageResponse
		if status == 200 {
			if json.Unmarshal(encoded, &result) != nil || result.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Pragma") != "no-cache" || response.ContentLength != int64(len(encoded)) {
				t.Fatal("目录成功 JSON 或缓存头错误")
			}
			if result.Data.NextCursor != nil {
				secrets = append(secrets, *result.Data.NextCursor)
			}
			return result
		}
		var body dto.ErrorResponse
		if json.Unmarshal(encoded, &body) != nil || body.Error.Code != code || body.RequestID != response.Header.Get("X-Request-ID") {
			t.Fatal("错误响应结构或业务码错误")
		}
		if field == "" {
			if body.Error.Details != nil {
				t.Fatal("非字段错误 details 非空")
			}
		} else if len(body.Error.Details) != 1 || body.Error.Details["field"] != field {
			t.Fatal("错误字段位置错误")
		}
		if (status == 401) != (response.Header.Get("WWW-Authenticate") != "") {
			t.Fatal("Bearer 提示与状态不匹配")
		}
		return result
	}
	caller, _ := newLogin(t, 1)
	foreign, _ := newLogin(t, 3)

	// 2. 真实认证后读取分页、路径与原始签名游标，继续下一页。
	first := request(t, caller, "parent_id=12&limit=2", "", 200, 1, "", "")
	if len(first.Data.Items) != 2 || first.Data.Items[0].ID != "23" || first.Data.Items[1].ID != "22" || first.Data.NextCursor == nil || len(first.Data.Breadcrumbs) != 3 || first.Data.Breadcrumbs[1].ID != "14" {
		t.Fatal("真实 HTTP 首页字段或路径错误")
	}
	next := request(t, caller, "parent_id=12&limit=2&cursor="+url.QueryEscape(*first.Data.NextCursor), "", 200, 1, "", "")
	if len(next.Data.Items) != 1 || next.Data.Items[0].ID != "21" || next.Data.NextCursor != nil {
		t.Fatal("真实 HTTP 末页错误")
	}
	empty := request(t, caller, "parent_id=15", "", 200, 1, "", "")
	if empty.Data.Items == nil || len(empty.Data.Items) != 0 || empty.Data.NextCursor != nil {
		t.Fatal("真实空目录响应错误")
	}
	root := request(t, caller, "parent_id=11", "", 200, 1, "", "")
	if len(root.Data.Breadcrumbs) != 1 || root.Data.Breadcrumbs[0].ID != "11" {
		t.Fatal("真实根目录响应错误")
	}

	// 3. 验证认证先行、查询错误与签名/目录目标错误分界。
	request(t, "", "parent_id=0", "", 401, 0, "AUTH_REQUIRED", "")
	request(t, caller, "parent_id=0", "", 400, 0, "INVALID_ARGUMENT", "parent_id")
	request(t, caller, "parent_id=12&scope=all", "", 400, 0, "INVALID_ARGUMENT", "scope")
	request(t, caller, "parent_id=12&cursor=a&cursor=b", "", 400, 0, "INVALID_ARGUMENT", "cursor")
	request(t, caller, "parent_id=12&note=%ZZ", "", 400, 0, "INVALID_ARGUMENT", "query")
	request(t, caller, "parent_id=12&cursor=private-invalid-cursor", "", 400, 1, "INVALID_CURSOR", "cursor")
	request(t, foreign, "parent_id=12&cursor="+url.QueryEscape(*first.Data.NextCursor), "", 400, 1, "INVALID_CURSOR", "cursor")
	request(t, caller, "parent_id=33", "", 404, 1, "ENTRY_NOT_FOUND", "")
	request(t, caller, "parent_id=18446744073709551615", "", 404, 1, "ENTRY_NOT_FOUND", "")
	request(t, caller, "parent_id=42", "", 400, 1, "INVALID_ARGUMENT", "parent_id")

	// 4. 目录与祖先生命周期、账号状态和旧凭证由真实数据库状态决定。
	for _, targetID := range []uint64{12, 14} {
		for _, status := range []string{"TRASHED", "PURGING", "PURGED"} {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,deleted_at=UTC_TIMESTAMP(6),purge_started_at=UTC_TIMESTAMP(6),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL) WHERE id=?", status, status, targetID); err != nil {
				t.Fatal(err)
			}
			request(t, caller, "parent_id=12", "", 404, 1, "ENTRY_NOT_FOUND", "")
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL WHERE id=?", targetID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	request(t, caller, "parent_id=0", "", 403, 0, "ACCOUNT_DISABLED", "")
	if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	request(t, caller, "parent_id=12", "disable-after-auth", 403, 1, "ACCOUNT_DISABLED", "")
	if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"revoked session", "expired session", "revoked device"} {
		token, actor := newLogin(t, 1)
		switch state {
		case "revoked session":
			if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", actor.SessionID); err != nil {
				t.Fatal(err)
			}
		case "expired session":
			if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 MINUTE WHERE id=?", actor.SessionID); err != nil {
				t.Fatal(err)
			}
		case "revoked device":
			if _, err := pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", actor.DeviceID); err != nil {
				t.Fatal(err)
			}
		}
		request(t, token, "parent_id=0", "", 401, 0, "SESSION_REVOKED", "")
	}

	// 5. 真实读取后的快照故障、签名故障、panic 和 DTO 越界使用统一内部错误。
	for _, mode := range []string{"snapshot-failure", "sign-failure", "panic"} {
		request(t, caller, "parent_id=12&limit=2", mode, 500, 1, "INTERNAL_ERROR", "")
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET file_size=9007199254740992 WHERE id=23"); err != nil {
		t.Fatal(err)
	}
	request(t, caller, "parent_id=12&limit=2", "", 500, 1, "INTERNAL_ERROR", "")
	server.Close()
	for _, secret := range append(secrets, "private-query", "private-cookie", "private-invalid-cursor", "private-snapshot-failure", "private-service-panic") {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("完成日志包含凭证、游标或内部故障原文")
		}
	}
}
