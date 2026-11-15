package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestEntryDetailHandlerIntegration(t *testing.T) {
	// 1. 复用正式表独占库，追加正式会话和偏好表，创建详情元数据。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	for _, boundary := range []struct{ table, next string }{
		{table: "auth_sessions", next: "stored_objects"},
		{table: "user_entry_preferences", next: "upload_tasks"},
	} {
		start := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.table+" (")
		if start < 0 {
			t.Fatal("未找到正式联测表")
		}
		end := strings.Index(migrations.InitialSQL[start:], "CREATE TABLE "+boundary.next+" (")
		if end < 0 {
			t.Fatal("未找到正式联测表结束边界")
		}
		if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:start+end]); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','详情处理用户','fixture_hash'),(3,'fixture_three','其他用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-detail-handler',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,color_key) VALUES(11,1,NULL,'',X'','DIR',NULL),(33,3,NULL,'',X'','DIR',NULL),(14,1,11,'祖先目录',CAST('祖先目录' AS BINARY),'DIR',NULL),(12,1,14,'当前目录',CAST('当前目录' AS BINARY),'DIR','sand')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,extension,mime_type,object_id,file_size,revision,source_device_name,content_modified_at) VALUES(23,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT','md','text/markdown',41,2048,3,'发布时设备 🎮','2026-11-15 02:30:00.123456')",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,23,'2026-11-15 02:30:00.123456','2026-11-15 02:30:01.123456')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("创建详情 HTTP 夹具失败", err)
		}
	}
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", stamp, stamp, stamp); err != nil {
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
			device, err := txStore.EnsureLoginDevice(txContext, uuid.NewString(), "详情 HTTP 测试", "WEB")
			if err != nil {
				return err
			}
			session, err = txStore.CreateLoginSession(txContext, device.ID, hash, time.Now().Add(time.Hour))
			return err
		}); err != nil || session == nil {
			t.Fatal("创建详情认证夹具失败", err)
		}
		token, err := tokenSigner.Issue(*session)
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, token.Token)
		return token.Token, domain.Actor{UserID: userID, SessionID: session.ID, DeviceID: session.DeviceID}
	}
	withSnapshot := func(ctx context.Context, action func(context.Context, service.EntryDetailReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
			return action(snapshotContext, snapshotStore)
		})
	}
	details := service.NewEntryDetailService(withSnapshot)
	type modeKey struct{}
	failure := errors.New("private-detail-query-failure")
	failingReads := service.NewEntryDetailService(func(ctx context.Context, action func(context.Context, service.EntryDetailReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
			// 复用既有联测读取器，在真实快照中模拟展示或偏好读取失败。
			failing := &entryDetailChangingReader{Store: snapshotStore}
			switch ctx.Value(modeKey{}) {
			case "display-failure":
				failing.displayError = failure
			case "preference-failure":
				failing.preferenceError = failure
			}
			if err := action(snapshotContext, failing); err != nil {
				return err
			}
			return failure
		})
	})
	var calls atomic.Int32
	var logs bytes.Buffer
	router := serverhttp.NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	router.GET("/api/v1/test-entry-details/:id", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure), func(requestContext *gin.Context) {
		requestContext.Request = requestContext.Request.WithContext(context.WithValue(requestContext.Request.Context(), modeKey{}, requestContext.GetHeader("X-Test-Mode")))
	}, handler.EntryDetail(func(ctx context.Context, actor domain.Actor, entryID uint64) (*domain.EntryDetail, error) {
		calls.Add(1)
		switch ctx.Value(modeKey{}) {
		case "display-failure", "preference-failure", "snapshot-failure":
			return failingReads.Get(ctx, actor, entryID)
		case "disable-after-auth":
			if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=?", actor.UserID); err != nil {
				return nil, err
			}
		case "panic":
			panic("private-detail-service-panic")
		}
		return details.Get(ctx, actor, entryID)
	}))
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	request := func(t *testing.T, token, path, mode string, status, serviceCalls int, code, field string) dto.EntryDetailResponse {
		t.Helper()
		before := calls.Load()
		message, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v1/test-entry-details/"+path+"?id=99&user_id=99&note=private-query", strings.NewReader(`{"id":"99","user_id":"99"}`))
		if err != nil {
			t.Fatal(err)
		}
		message.Header.Set("Origin", "http://127.0.0.1:5173")
		message.Header.Set("X-Request-ID", "28EE758A-0727-42DE-AE7D-3BB33155E148")
		message.Header.Set("X-Test-Mode", mode)
		message.Header.Set("X-User-ID", "99")
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
		if readError != nil || closeError != nil || response.StatusCode != status || int(calls.Load()-before) != serviceCalls ||
			response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Content-Encoding") != "" ||
			response.Header.Get("X-Request-ID") != "28ee758a-0727-42de-ae7d-3bb33155e148" || response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || len(response.Cookies()) != 0 {
			t.Fatal("详情网络状态、服务调用或响应头错误", response.StatusCode, calls.Load()-before, readError, closeError)
		}
		var result dto.EntryDetailResponse
		if status == 200 {
			if json.Unmarshal(encoded, &result) != nil || result.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Pragma") != "no-cache" || response.ContentLength != int64(len(encoded)) {
				t.Fatal("详情成功 JSON 或正文头错误")
			}
			return result
		}
		var body dto.ErrorResponse
		if json.Unmarshal(encoded, &body) != nil || body.Error.Code != code || body.RequestID != response.Header.Get("X-Request-ID") {
			t.Fatal("详情错误结构或业务码错误")
		}
		if field == "" {
			if body.Error.Details != nil {
				t.Fatal("非字段错误 details 非空")
			}
		} else if len(body.Error.Details) != 1 || body.Error.Details["field"] != field {
			t.Fatal("详情错误字段定位错误")
		}
		if code == "ENTRY_NOT_FOUND" && body.Error.Message != "目录项不存在" {
			t.Fatal("不可见目标提示错误")
		}
		if (status == 401) != (response.Header.Get("WWW-Authenticate") != "") {
			t.Fatal("详情认证提示与状态不匹配")
		}
		return result
	}
	caller, callerActor := newLogin(t, 1)
	foreign, _ := newLogin(t, 3)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET source_device_id=? WHERE id=23", callerActor.DeviceID); err != nil {
		t.Fatal(err)
	}

	// 2. 真实认证后取得文件、目录、根目录和其他账号自己的详情。
	file := request(t, caller, "23", "", 200, 1, "", "")
	if file.Data.ID != "23" || file.Data.ParentID == nil || *file.Data.ParentID != "12" || file.Data.Kind != "document" || file.Data.Revision != "3" || file.Data.SizeBytes != 2048 ||
		file.Data.SourceDevice == nil || *file.Data.SourceDevice.ID != strconv.FormatUint(callerActor.DeviceID, 10) || *file.Data.SourceDevice.Name != "发布时设备 🎮" || !file.Data.Starred ||
		!file.Data.ModifiedAt.Equal(stamp) || !file.Data.ContentModifiedAt.Equal(stamp) || !file.Data.StarredAt.Equal(stamp) || !file.Data.LastOpenedAt.Equal(stamp.Add(time.Second)) {
		t.Fatal("真实详情 HTTP 基础、展示或偏好错误")
	}
	directory := request(t, caller, "12", "", 200, 1, "", "")
	root := request(t, caller, "11", "", 200, 1, "", "")
	other := request(t, foreign, "33", "", 200, 1, "", "")
	if directory.Data.Kind != "folder" || directory.Data.ColorKey == nil || *directory.Data.ColorKey != "sand" || directory.Data.Starred || root.Data.ParentID != nil || root.Data.Name != "" || other.Data.ID != "33" {
		t.Fatal("真实目录、根目录或账号隔离错误")
	}

	// 3. 认证先于路径解析，目标缺失及跨账号使用同一公开错误。
	request(t, "", "0", "", 401, 0, "AUTH_REQUIRED", "")
	request(t, "private-invalid-access", "0", "", 401, 0, "AUTH_REQUIRED", "")
	for _, path := range []string{"0", "023", "18446744073709551616", "%2532%2533"} {
		request(t, caller, path, "", 400, 0, "INVALID_ARGUMENT", "id")
	}
	for _, path := range []string{"33", "999", "18446744073709551615"} {
		request(t, caller, path, "", 404, 1, "ENTRY_NOT_FOUND", "")
	}
	request(t, foreign, "23", "", 404, 1, "ENTRY_NOT_FOUND", "")

	// 4. 目标及两级祖先的生命周期和当前身份状态由真实数据库决定。
	for _, targetID := range []uint64{23, 12, 14} {
		for _, state := range []string{"TRASHED", "PURGING", "PURGED"} {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,object_id=IF(entry_type='FILE' AND ?<>'PURGED',41,NULL),deleted_at=UTC_TIMESTAMP(6),purge_started_at=UTC_TIMESTAMP(6),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL) WHERE id=?", state, state, state, targetID); err != nil {
				t.Fatal(err)
			}
			request(t, caller, "23", "", 404, 1, "ENTRY_NOT_FOUND", "")
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',object_id=IF(entry_type='FILE',41,NULL),deleted_at=NULL,purge_started_at=NULL,purged_at=NULL WHERE id=?", targetID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	request(t, caller, "0", "", 403, 0, "ACCOUNT_DISABLED", "")
	if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	request(t, caller, "23", "disable-after-auth", 403, 1, "ACCOUNT_DISABLED", "")
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
		request(t, token, "0", "", 401, 0, "SESSION_REVOKED", "")
	}

	// 5. 真实快照中的查询故障、结束故障、panic 和公开字节越界进入统一内部错误。
	for _, mode := range []string{"display-failure", "preference-failure", "snapshot-failure", "panic"} {
		request(t, caller, "23", mode, 500, 1, "INTERNAL_ERROR", "")
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET file_size=9007199254740992 WHERE id=23"); err != nil {
		t.Fatal(err)
	}
	request(t, caller, "23", "", 500, 1, "INTERNAL_ERROR", "")
	server.Close()
	for _, secret := range append(secrets, "private-query", "private-cookie", "private-invalid-access", "private-detail-query-failure", "private-detail-service-panic", "学习笔记.md", "发布时设备 🎮") {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("详情完成日志包含凭证、请求、展示或故障原文")
		}
	}
}
