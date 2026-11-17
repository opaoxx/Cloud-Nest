package repository

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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

func TestUploadTaskHandlerIntegration(t *testing.T) {
	// 1. 复用正式任务夹具，安装正式认证会话表并建立真实 JWT 授权链。
	pool, want := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("正式会话表定义未找到")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"UPDATE users SET status='ACTIVE' WHERE id=8",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(15,7,'68ee758a-0727-42de-ae7d-3bb33155e148','other device','ANDROID')",
		"INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at) VALUES(20,8,12,24,14,'foreign.bin','SINGLE',0,'UPLOADING',0,'2026-11-18 08:00:00.123456')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	configuration := config.JWTConfig{SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "cloudnest-api-test", AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour}
	signer, err := service.NewAccessTokenSigner(configuration)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewAccessTokenVerifier(configuration)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
	var secrets []string
	newToken := func(t *testing.T, userID, deviceID, sessionID uint64) string {
		t.Helper()
		digest := sha256.Sum256([]byte(strconv.FormatUint(sessionID, 10)))
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,?,?)", sessionID, userID, deviceID, digest[:], expiresAt); err != nil {
			t.Fatal(err)
		}
		token, err := signer.Issue(domain.LoginSession{ID: sessionID, UserID: userID, DeviceID: deviceID, ExpiresAt: expiresAt})
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, token.Token)
		return token.Token
	}
	ownerToken := newToken(t, 7, 11, 17)
	otherDeviceToken := newToken(t, 7, 15, 18)
	foreignToken := newToken(t, 8, 12, 19)
	authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
	tasks := service.NewUploadTaskService(store.FindUserUploadTask)
	type modeKey struct{}
	var serviceCalls atomic.Int32
	var logs bytes.Buffer
	const origin = "http://127.0.0.1:5173"
	router := serverhttp.NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
	router.GET("/api/v1/test-upload-tasks/:id", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure), func(requestContext *gin.Context) {
		requestContext.Request = requestContext.Request.WithContext(context.WithValue(requestContext.Request.Context(), modeKey{}, requestContext.GetHeader("X-Test-Mode")))
	}, handler.UploadTask(func(queryContext context.Context, actor domain.Actor, taskID uint64) (*domain.UploadTask, error) {
		serviceCalls.Add(1)
		if queryContext.Value(modeKey{}) == "query-failure" {
			return nil, errors.New("private-upload-query-failure")
		}
		return tasks.Get(queryContext, actor, taskID)
	}))
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	sequence := 0
	request := func(t *testing.T, token, path, mode string, status, expectedCalls int, code, field string) dto.UploadTaskResponse {
		t.Helper()
		sequence++
		before := serviceCalls.Load()
		message, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v1/test-upload-tasks/"+path+"?id=99&user_id=8&note=private-query", strings.NewReader(`{"id":"20","user_id":"8","note":"private-body"}`))
		if err != nil {
			t.Fatal(err)
		}
		requestID := "28ee758a-0727-42de-ae7d-" + strconv.FormatInt(300000000000+int64(sequence), 10)
		message.Header.Set("Origin", origin)
		message.Header.Set("X-Request-ID", strings.ToUpper(requestID))
		message.Header.Set("X-Test-Mode", mode)
		message.Header.Set("X-User-ID", "8")
		message.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-cookie"})
		if token != "" {
			message.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(message)
		if err != nil {
			t.Fatal("真实任务查询请求失败", err)
		}
		encoded, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if readError != nil || closeError != nil || response.StatusCode != status || int(serviceCalls.Load()-before) != expectedCalls ||
			response.ContentLength != int64(len(encoded)) || response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
			response.Header.Get("Content-Encoding") != "" || response.Header.Get("Cache-Control") != "no-store" ||
			response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Access-Control-Allow-Origin") != origin || len(response.Cookies()) != 0 {
			t.Fatal("真实任务查询状态、调用次数或响应头错误", response.StatusCode, readError, closeError)
		}
		if strings.Contains(string(encoded), "private-") || strings.Contains(string(encoded), "曾经重试") {
			t.Fatal("真实任务查询响应包含请求或内部原文")
		}
		if (status == 401) != (response.Header.Get("WWW-Authenticate") == `Bearer realm="cloudnest"`) {
			t.Fatal("真实任务查询认证提示错误")
		}
		if status != 200 {
			var failure dto.ErrorResponse
			if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != requestID {
				t.Fatal("真实任务查询错误结构错误")
			}
			if field != "" {
				if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
					t.Fatal("真实任务查询字段错误定位失败")
				}
			} else if failure.Error.Details != nil {
				t.Fatal("真实任务查询错误包含额外详情")
			}
			return dto.UploadTaskResponse{}
		}
		var result dto.UploadTaskResponse
		if json.Unmarshal(encoded, &result) != nil || result.RequestID != requestID || response.Header.Get("Pragma") != "no-cache" {
			t.Fatal("真实任务查询成功 JSON 错误")
		}
		return result
	}

	// 2. 真实授权先于路径及服务，成功结果按用户范围交付。
	t.Run("authentication and path boundaries", func(t *testing.T) {
		request(t, "", "0", "", 401, 0, "AUTH_REQUIRED", "")
		request(t, "invalid-token", "0", "", 401, 0, "AUTH_REQUIRED", "")
		request(t, ownerToken, "0", "", 400, 0, "INVALID_ARGUMENT", "id")
		request(t, ownerToken, "019", "", 400, 0, "INVALID_ARGUMENT", "id")
		request(t, ownerToken, "18446744073709551616", "", 400, 0, "INVALID_ARGUMENT", "id")
		request(t, ownerToken, "%2531%2539", "", 400, 0, "INVALID_ARGUMENT", "id")
		result := request(t, ownerToken, "%31%39", "", 200, 1, "", "")
		if result.Data.ID != "19" || result.Data.ParentID != "13" || result.Data.RequestedName != want.RequestedName {
			t.Fatal("真实任务查询路径、归属或展示字段错误")
		}
		request(t, ownerToken, "999", "", 404, 1, "UPLOAD_NOT_FOUND", "")
		request(t, ownerToken, "20", "", 404, 1, "UPLOAD_NOT_FOUND", "")
		request(t, foreignToken, "19", "", 404, 1, "UPLOAD_NOT_FOUND", "")
		if result := request(t, otherDeviceToken, "19", "", 200, 1, "", ""); result.Data.ID != "19" {
			t.Fatal("同用户跨设备任务查询失败")
		}
		request(t, ownerToken, "19", "query-failure", 500, 1, "INTERNAL_ERROR", "")
	})
	for _, status := range []domain.UploadStatus{
		domain.UploadStatusCreating, domain.UploadStatusUploading, domain.UploadStatusCompleting,
		domain.UploadStatusVerifying, domain.UploadStatusCompleted, domain.UploadStatusCancelling,
		domain.UploadStatusCancelled, domain.UploadStatusExpiring, domain.UploadStatusExpired, domain.UploadStatusFailed,
	} {
		t.Run(string(status), func(t *testing.T) {
			if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status=? WHERE user_id=7 AND id=19", string(status)); err != nil {
				t.Fatal(err)
			}
			if result := request(t, ownerToken, "19", "", 200, 1, "", ""); result.Data.Status != string(status) {
				t.Fatal("真实任务查询状态错误")
			}
		})
	}

	// 3. 每次请求复查真实账号、会话和设备的授权状态。
	t.Run("current authorization state", func(t *testing.T) {
		for _, fixture := range []struct {
			update, restore, code string
			status                int
		}{
			{"UPDATE users SET status='DISABLED' WHERE id=7", "UPDATE users SET status='ACTIVE' WHERE id=7", "ACCOUNT_DISABLED", 403},
			{"UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17", "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17", "SESSION_REVOKED", 401},
			{"UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=11", "UPDATE user_devices SET revoked_at=NULL WHERE id=11", "SESSION_REVOKED", 401},
		} {
			if _, err := pool.ExecContext(ctx, fixture.update); err != nil {
				t.Fatal(err)
			}
			request(t, ownerToken, "19", "", fixture.status, 0, fixture.code, "")
			if _, err := pool.ExecContext(ctx, fixture.restore); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET expires_at=? WHERE id=17", expiresAt.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		request(t, ownerToken, "19", "", 401, 0, "SESSION_REVOKED", "")
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET expires_at=? WHERE id=17", expiresAt); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unsafe public size", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET file_size=9007199254740992 WHERE user_id=7 AND id=19"); err != nil {
			t.Fatal(err)
		}
		request(t, ownerToken, "19", "", 500, 1, "INTERNAL_ERROR", "")
	})

	// 4. 最大标识最后写入，贯穿 JWT、数据库授权、路径解析及任务响应。
	t.Run("full unsigned identity and task", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'upload_http_max','完整整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'78ee758a-0727-42de-ae7d-3bb33155e148','maximum','WEB')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(?,?,'MINIO','fixture-bucket','handler-maximum',2048)", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO upload_tasks(id,user_id,device_id,staged_object_id,target_parent_id,requested_name,upload_mode,file_size,status,reserved_bytes,expires_at) VALUES(?,?,?,?,?,'maximum.bin','SINGLE',2048,'UPLOADING',2048,?)", maximum, maximum, maximum, maximum, maximum, want.ExpiresAt); err != nil {
			t.Fatal(err)
		}
		token := newToken(t, maximum, maximum, maximum)
		result := request(t, token, strconv.FormatUint(maximum, 10), "", 200, 1, "", "")
		if result.Data.ID != "18446744073709551615" || result.Data.ParentID != result.Data.ID {
			t.Fatal("完整无符号任务或目录 ID 交付错误")
		}
	})
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != sequence || sequence != 28 {
		t.Fatal("真实任务查询次数或完成日志数错误", sequence)
	}
	for _, marker := range append(secrets, "private-", "学习笔记", "发布笔记", "曾经重试") {
		if strings.Contains(logs.String(), marker) {
			t.Fatal("真实任务查询日志包含凭证、请求或任务原文")
		}
	}
}
