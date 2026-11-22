package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
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
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestCreateDirectoryHandlerIntegration(t *testing.T) {
	// 1. 使用正式表独占库，装配真实 JWT、用户事务、幂等服务和测试 POST 路由。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	installChangeEventFixtureTable(t, ctx, pool)
	sessionStart := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	sessionEnd := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	idempotencyStart := strings.Index(migrations.InitialSQL, "CREATE TABLE idempotency_records (")
	if sessionStart < 0 || sessionEnd <= sessionStart || idempotencyStart < 0 {
		t.Fatal("正式表边界错误")
	}
	for _, statement := range []string{
		migrations.InitialSQL[sessionStart:sessionEnd], migrations.InitialSQL[idempotencyStart:],
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','创建 HTTP 用户','fixture_hash','ACTIVE'),(3,'fixture_three','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-create-http',1)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(15,1,11,'父项文件',CAST('父项文件' AS BINARY),'FILE','OTHER',41,1)",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','创建 HTTP 设备','WEB')",
		"INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)",
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
	access, err := signer.Issue(domain.LoginSession{ID: 17, UserID: 1, DeviceID: 13, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
	var failEncode, disableAfterAuthorization atomic.Bool
	creator := service.NewIdempotentCreateDirectoryService(
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentCreateDirectoryTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
		},
		func(entry *domain.FileEntry, color *string) (json.RawMessage, error) {
			if failEncode.Swap(false) {
				return nil, errors.New("private-create-encoding-failure")
			}
			data, err := dto.NewCreateDirectoryData(entry, color)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		},
		func(body json.RawMessage) error { _, err := dto.RestoreCreateDirectoryData(body); return err },
	)
	var logs bytes.Buffer
	router := serverhttp.NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	router.POST("/test-create-directory", middleware.RequireAuthorization(func(ctx context.Context, token string) (*domain.Actor, error) {
		actor, err := authorization.Authorize(ctx, token)
		if err == nil && disableAfterAuthorization.Swap(false) {
			_, err = pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1")
		}
		return actor, err
	}, handler.AuthorizationFailure), handler.CreateDirectory(creator.Create))
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	counts := func() [2]int {
		t.Helper()
		var result [2]int
		if err := pool.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM file_entries),(SELECT COUNT(*) FROM idempotency_records)").Scan(&result[0], &result[1]); err != nil {
			t.Fatal(err)
		}
		return result
	}
	requestNumber := 0
	request := func(t *testing.T, body, key, token string, status int, code, field string) dto.CreateDirectoryResponse {
		t.Helper()
		requestNumber++
		requestID := fmt.Sprintf("28ee758a-0727-42de-ae7d-%012d", requestNumber)
		message, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/test-create-directory?note=private-query", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		message.Header.Set("Content-Type", "application/json")
		message.Header.Set("Origin", "http://127.0.0.1:5173")
		message.Header.Set("X-Request-ID", requestID)
		message.Header.Set("X-User-ID", "99")
		message.AddCookie(&http.Cookie{Name: handler.RefreshCookieName, Value: "private-cookie"})
		if key != "" {
			message.Header.Set("Idempotency-Key", key)
		}
		if token != "" {
			message.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(message)
		if err != nil {
			t.Fatal("创建 HTTP 请求失败", err)
		}
		encoded, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if readError != nil || closeError != nil || response.StatusCode != status || response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
			response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Encoding") != "" || response.Header.Get("X-Request-ID") != requestID ||
			response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || len(response.Cookies()) != 0 || response.ContentLength != int64(len(encoded)) {
			t.Fatal("创建 HTTP 状态、正文或响应头错误", response.StatusCode, readError, closeError)
		}
		var result dto.CreateDirectoryResponse
		if status == 201 {
			if json.Unmarshal(encoded, &result) != nil || result.RequestID != requestID || response.Header.Get("Pragma") != "no-cache" {
				t.Fatal("创建成功 JSON 错误")
			}
			return result
		}
		var failure dto.ErrorResponse
		if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != requestID {
			t.Fatal("创建错误结构错误")
		}
		if field == "" {
			if failure.Error.Details != nil {
				t.Fatal("普通错误 details 非空")
			}
		} else if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
			t.Fatal("错误字段定位错误")
		}
		if (status == 401) != (response.Header.Get("WWW-Authenticate") != "") {
			t.Fatal("认证提示错误")
		}
		return result
	}
	const body = `{"parent_id":"11","name":"学习资料 🌱","color_key":"green"}`

	// 2. 实际创建、重试及目录改名后复用创建快照，当前请求 ID 分别关联。
	t.Run("create replay snapshot and ownership", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(0)
		before := counts()
		first := request(t, body, "Request-007", access.Token, 201, "", "")
		retry := request(t, `{"color_key":"green","name":"  学习资料 🌱  ","parent_id":"11"}`, "Request-007", access.Token, 201, "", "")
		if !reflect.DeepEqual(first.Data, retry.Data) || first.RequestID == retry.RequestID || counts() != [2]int{before[0] + 1, before[1] + 1} {
			t.Fatal("首次与重试结果不一致或重复写入")
		}
		var owner uint64
		if err := pool.QueryRowContext(ctx, "SELECT user_id FROM file_entries WHERE id=?", first.Data.Entry.ID).Scan(&owner); err != nil || owner != 1 {
			t.Fatal("创建身份被请求头覆盖", err)
		}
		newKey, err := service.BuildEntryNameKey("后续名称")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name='后续名称',name_key=?,color_key='sand',revision=2 WHERE id=?", newKey, first.Data.Entry.ID); err != nil {
			t.Fatal(err)
		}
		retry = request(t, body, "Request-007", access.Token, 201, "", "")
		if !reflect.DeepEqual(first.Data, retry.Data) {
			t.Fatal("HTTP 复用混入当前目录变更")
		}
	})
	t.Run("input and authorization order", func(t *testing.T) {
		before := counts()
		request(t, `{"private-broken":`, "", "", 401, "AUTH_REQUIRED", "")
		request(t, `{"private-broken":`, "", access.Token, 400, "INVALID_ARGUMENT", "Idempotency-Key")
		request(t, `{"parent_id":"11","name":"资料","user_id":"99"}`, "private-key", access.Token, 400, "INVALID_ARGUMENT", "")
		request(t, `{"parent_id":"11","name":"CON.txt"}`, "invalid-name", access.Token, 400, "INVALID_NAME", "name")
		if counts() != before {
			t.Fatal("非法输入产生数据")
		}
	})
	t.Run("parent scope and type", func(t *testing.T) {
		before := counts()
		for _, parent := range []string{"33", "999999", "15"} {
			status, code, field := 404, "ENTRY_NOT_FOUND", ""
			if parent == "15" {
				status, code, field = 400, "INVALID_ARGUMENT", "parent_id"
			}
			request(t, `{"parent_id":"`+parent+`","name":"新目录"}`, "parent-"+parent, access.Token, status, code, field)
		}
		if counts() != before {
			t.Fatal("不可用父项产生数据")
		}
	})
	t.Run("conflicts and optional colors", func(t *testing.T) {
		before := counts()
		request(t, `{"parent_id":"11","name":"不同名称"}`, "Request-007", access.Token, 409, "IDEMPOTENCY_CONFLICT", "")
		if counts() != before {
			t.Fatal("参数冲突产生数据")
		}
		first := request(t, `{"parent_id":"11","name":"默认颜色"}`, "default-color", access.Token, 201, "", "")
		retry := request(t, `{"parent_id":"11","name":"默认颜色","color_key":null}`, "default-color", access.Token, 201, "", "")
		if first.Data.Entry.ColorKey != nil || !reflect.DeepEqual(first.Data, retry.Data) {
			t.Fatal("省略与 null 颜色未准确复用")
		}
		request(t, `{"parent_id":"11","name":"默认颜色","color_key":""}`, "default-color", access.Token, 409, "IDEMPOTENCY_CONFLICT", "")
		request(t, `{"parent_id":"11","name":"默认颜色"}`, "another-key", access.Token, 409, "NAME_CONFLICT", "")
	})

	// 3. 认证后状态变化与编码故障通过真实事务复查及回滚。
	t.Run("identity changes after authorization", func(t *testing.T) {
		before := counts()
		disableAfterAuthorization.Store(true)
		request(t, body, "Request-007", access.Token, 403, "ACCOUNT_DISABLED", "")
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if counts() != before {
			t.Fatal("身份复查失败产生数据")
		}
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17"); err != nil {
			t.Fatal(err)
		}
		request(t, body, "Request-007", access.Token, 401, "SESSION_REVOKED", "")
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=NULL WHERE id=17"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("encoding failure rollback", func(t *testing.T) {
		before := counts()
		failEncode.Store(true)
		request(t, `{"parent_id":"11","name":"编码回滚"}`, "encoding-rollback", access.Token, 500, "INTERNAL_ERROR", "")
		if counts() != before {
			t.Fatal("HTTP 编码失败未回滚双表")
		}
	})
	t.Run("expired and corrupt records", func(t *testing.T) {
		before := counts()
		if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET created_at=UTC_TIMESTAMP(6)-INTERVAL 9 DAY,expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 DAY WHERE user_id=1 AND idempotency_key='Request-007'"); err != nil {
			t.Fatal(err)
		}
		request(t, body, "Request-007", access.Token, 409, "IDEMPOTENCY_EXPIRED", "")
		if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=JSON_OBJECT('entry',JSON_OBJECT('id',CAST(resource_id AS CHAR))) WHERE user_id=1 AND idempotency_key='default-color'"); err != nil {
			t.Fatal(err)
		}
		request(t, `{"parent_id":"11","name":"默认颜色"}`, "default-color", access.Token, 500, "INTERNAL_ERROR", "")
		if counts() != before {
			t.Fatal("到期或异常记录产生新数据")
		}
	})
	t.Run("high unsigned id", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "ALTER TABLE file_entries AUTO_INCREMENT=9223372036854776000"); err != nil {
			t.Fatal(err)
		}
		result := request(t, `{"parent_id":"11","name":"高位 ID"}`, "high-id", access.Token, 201, "", "")
		if result.Data.Entry.ID != "9223372036854776000" {
			t.Fatal("HTTP 高位 ID 丢失精度")
		}
	})
	for _, marker := range []string{access.Token, "private-query", "private-cookie", "private-key", "private-create-encoding-failure", "学习资料", "默认颜色", "Request-007"} {
		if strings.Contains(logs.String(), marker) {
			t.Fatal("完成日志包含原始请求、凭证或正文")
		}
	}
}
