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
	"strconv"
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

func TestCreateUploadHandlerIntegration(t *testing.T) {
	// 1. 安装正式表，准备真实 JWT、授权状态和上传父目录。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(3)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, boundary := range []struct{ table, next string }{{"auth_sessions", "stored_objects"}, {"upload_tasks", "idempotency_records"}, {"idempotency_records", ""}} {
		start := strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.table+" (")
		end := len(migrations.InitialSQL)
		if boundary.next != "" {
			end = strings.Index(migrations.InitialSQL, "CREATE TABLE "+boundary.next+" (")
		}
		if start < 0 || end <= start {
			t.Fatal("正式上传处理联测表边界错误")
		}
		if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:end]); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,storage_quota,used_storage) VALUES(1,'upload_handler','上传处理用户','fixture_hash',104857600,1),(3,'upload_handler_other','另一用户','fixture_hash',104857600,0)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'local_minio','cloudnest','fixture/parent-file',1)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(15,1,11,'parent.bin',X'706172656e742e62696e','FILE','OTHER',41,1)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(16,1,11,'hidden',X'68696464656e','DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(17,1,16,'nested',X'6e6573746564','DIR')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','上传 HTTP 设备','WEB')",
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
	creator := service.NewIdempotentCreateUploadService("local_minio", "cloudnest", func(ctx context.Context, current domain.Actor, action func(context.Context, service.IdempotentCreateUploadTransaction) error) error {
		return store.WithUserTransaction(ctx, current, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
	}, func(task *domain.UploadTask) (json.RawMessage, error) {
		if failEncode.Swap(false) {
			return nil, errors.New("private-upload-encoding-failure")
		}
		if task.UploadMode == "MULTIPART" {
			data, err := dto.NewCreateMultipartUploadData(task)
			if err != nil {
				return nil, err
			}
			return json.Marshal(data)
		}
		data, err := dto.NewCreateUploadData(task)
		if err != nil {
			return nil, err
		}
		return json.Marshal(data)
	}, func(body json.RawMessage) error {
		var mode struct {
			UploadMode string `json:"upload_mode"`
		}
		if err := json.Unmarshal(body, &mode); err != nil {
			return err
		}
		if mode.UploadMode == "MULTIPART" {
			_, err := dto.RestoreCreateMultipartUploadData(body)
			return err
		}
		_, err := dto.RestoreCreateUploadData(body)
		return err
	})
	var logs bytes.Buffer
	const origin = "http://127.0.0.1:5173"
	router := serverhttp.NewRouter(logging.New(&logs), []string{origin}, func(context.Context) bool { return true })
	authorize := middleware.RequireAuthorization(func(ctx context.Context, token string) (*domain.Actor, error) {
		actor, err := authorization.Authorize(ctx, token)
		if err == nil && disableAfterAuthorization.Swap(false) {
			_, err = pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1")
		}
		return actor, err
	}, handler.AuthorizationFailure)
	const singlePath, plannedPath = "/test-create-upload", "/test-create-upload-with-plan"
	router.POST(singlePath, authorize, handler.CreateUpload(creator.Create))
	const fixtureLimit = 20 * 1024 * 1024
	limits := domain.UploadPlanLimits{
		MaxFileSizeBytes: domain.DefaultMaxUploadSizeBytes, MaxObjectSizeBytes: fixtureLimit, MaxPartSizeBytes: 64 * 1024 * 1024,
	}
	router.POST(plannedPath, authorize, handler.CreateUploadWithMultipart(func(ctx context.Context, actor domain.Actor, input domain.CreateUploadInput, key string) (json.RawMessage, error) {
		return creator.CreateWithPlan(ctx, actor, input, key, limits)
	}))
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	type uploadState struct{ reserved, taskReserved, objects, tasks, records uint64 }
	readState := func() uploadState {
		var state uploadState
		if err := pool.QueryRowContext(ctx, "SELECT reserved_storage,(SELECT COALESCE(SUM(reserved_bytes),0) FROM upload_tasks WHERE user_id=1),(SELECT COUNT(*) FROM stored_objects WHERE owner_user_id=1),(SELECT COUNT(*) FROM upload_tasks WHERE user_id=1),(SELECT COUNT(*) FROM idempotency_records WHERE user_id=1) FROM users WHERE id=1").Scan(&state.reserved, &state.taskReserved, &state.objects, &state.tasks, &state.records); err != nil {
			t.Fatal(err)
		}
		return state
	}
	requestNumber := 0
	var requestPaths []string
	send := func(path, body, key, token string, status int) ([]byte, string) {
		t.Helper()
		requestNumber++
		requestPaths = append(requestPaths, path)
		requestID := fmt.Sprintf("28ee758a-0727-42de-ae7d-%012d", requestNumber)
		message, err := http.NewRequestWithContext(ctx, "POST", server.URL+path+"?note=private-query", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		message.Header.Set("Content-Type", "application/json")
		message.Header.Set("Origin", origin)
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
			t.Fatal("上传创建 HTTP 请求失败", err)
		}
		encoded, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if readError != nil || closeError != nil || response.StatusCode != status || response.ContentLength != int64(len(encoded)) || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Content-Encoding") != "" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Access-Control-Allow-Origin") != origin || len(response.Cookies()) != 0 {
			t.Fatal("上传创建 HTTP 状态、头或正文长度错误", response.StatusCode, readError, closeError)
		}
		if status == http.StatusCreated && response.Header.Get("Pragma") != "no-cache" {
			t.Fatal("创建回执缺少 Pragma")
		}
		return encoded, requestID
	}
	request := func(body, key, token string, status int, code, field string) dto.CreateUploadResponse {
		t.Helper()
		encoded, requestID := send(singlePath, body, key, token, status)
		if status == 201 {
			var result dto.CreateUploadResponse
			if json.Unmarshal(encoded, &result) != nil || result.RequestID != requestID {
				t.Fatal("上传创建成功 JSON 错误")
			}
			return result
		}
		var failure dto.ErrorResponse
		if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != requestID {
			t.Fatal("上传创建错误结构错误", string(encoded))
		}
		if code == "FILE_TOO_LARGE" {
			if len(failure.Error.Details) != 2 || failure.Error.Details["field"] != "size_bytes" || failure.Error.Details["max_size_bytes"] != float64(domain.MaxSingleUploadSizeBytes) {
				t.Fatal("超限响应缺少当前上限")
			}
		} else if field != "" {
			if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
				t.Fatal("错误字段定位错误")
			}
		} else if failure.Error.Details != nil {
			t.Fatal("普通错误 details 应为 null")
		}
		return dto.CreateUploadResponse{}
	}
	const ordinary = `{"parent_id":"11","name":"学习.bin","size_bytes":40}`

	// 2. 真实客户端请求通过认证、参数解析、幂等服务和成功交付。
	t.Run("create replay declarations and historical result", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		before := readState()
		first := request(ordinary, "create-ordinary", access.Token, 201, "", "")
		after := readState()
		if after.reserved != before.reserved+40 || after.reserved != after.taskReserved || after.objects != before.objects+1 || after.tasks != before.tasks+1 || after.records != before.records+1 {
			t.Fatal("HTTP 创建未整体提交四项事实")
		}
		second := request(`{"size_bytes":40,"name":"  学习.bin  ","sha256":null,"parent_id":"11"}`, "create-ordinary", access.Token, 201, "", "")
		if !reflect.DeepEqual(first.Data, second.Data) || first.RequestID == second.RequestID || readState() != after {
			t.Fatal("HTTP 重试未复用快照或重复预占")
		}
		for _, fixture := range []struct {
			key, body string
			size      uint64
		}{
			{"zero", `{"parent_id":"11","name":"empty.bin","size_bytes":0}`, 0},
			{"limit", `{"parent_id":"11","name":"limit.bin","size_bytes":16777216}`, domain.MaxSingleUploadSizeBytes},
			{"digest", `{"parent_id":"11","name":"digest.bin","size_bytes":40,"sha256":"` + strings.Repeat("aB", 32) + `"}`, 40},
		} {
			result := request(fixture.body, fixture.key, access.Token, 201, "", "")
			if result.Data.Task.SizeBytes != fixture.size {
				t.Fatal("公开声明大小错误")
			}
		}
		if _, err := pool.ExecContext(ctx, "UPDATE upload_tasks SET status='VERIFYING',expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=?", first.Data.Task.ID); err != nil {
			t.Fatal(err)
		}
		before = readState()
		historical := request(ordinary, "create-ordinary", access.Token, 201, "", "")
		if !reflect.DeepEqual(first.Data, historical.Data) || readState() != before {
			t.Fatal("历史结果未准确复用")
		}
		taskID, err := strconv.ParseUint(first.Data.Task.ID, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		task, err := store.FindUserUploadTask(ctx, 1, taskID)
		if err != nil || task.UserID != 1 || task.DeviceID != 13 {
			t.Fatal("HTTP 头影响了可信用户归属", err)
		}
	})
	t.Run("multipart HTTP snapshot and errors", func(t *testing.T) {
		const key = "multipart-http"
		base := `{"parent_id":"11","name":"  分片夹具.bin  ","size_bytes":16777217`
		digestField := `,"sha256":"` + strings.Repeat("aB", 32) + `"}`
		body := base + digestField
		readResult := func(encoded []byte, requestID string) json.RawMessage {
			t.Helper()
			var response struct {
				Data      json.RawMessage `json:"data"`
				RequestID string          `json:"request_id"`
			}
			if json.Unmarshal(encoded, &response) != nil || response.RequestID != requestID {
				t.Fatal("分片回执缺少当前请求 ID")
			}
			data, err := dto.RestoreCreateMultipartUploadData(response.Data)
			if err != nil || data.Task.RequestedName != "分片夹具.bin" || data.Task.ParentID != "11" ||
				data.Task.SizeBytes != 16777217 || data.PartCount != 2 || data.PartSizeBytes != 16777216 ||
				data.Task.Status != "CREATING" || !data.ExpiresAt.Equal(data.Task.ExpiresAt) {
				t.Fatalf("分片公开快照不符: %+v %v", data, err)
			}
			return response.Data
		}

		// 1. 通过真正认证和幂等事务创建再重试，保留原始快照。
		before := readState()
		first, firstID := send(plannedPath, body, key, access.Token, 201)
		firstData := readResult(first, firstID)
		state := readState()
		if state.reserved != before.reserved+16777217 || state.reserved != state.taskReserved ||
			state.tasks != before.tasks+1 || state.objects != before.objects+1 || state.records != before.records+1 {
			t.Fatal("分片 HTTP 创建未提交四项事实")
		}
		replayed, nextID := send(plannedPath, body, key, access.Token, 201)
		if firstID == nextID || !bytes.Equal(firstData, readResult(replayed, nextID)) || readState() != state {
			t.Fatal("重试未保留原快照、使用了旧请求 ID 或重复创建")
		}

		// 2. 直接验证公开错误及数据库无新增事实。
		for _, fixture := range []struct {
			body, key, token, code string
			status                 int
			details                map[string]any
		}{
			{base + "}", "multipart-missing-hash", access.Token, "INVALID_ARGUMENT", 400, map[string]any{"field": "sha256"}},
			{base + `,"sha256":null}`, "multipart-null-hash", access.Token, "INVALID_ARGUMENT", 400, map[string]any{"field": "sha256"}},
			{strings.Replace(body, "16777217", "20971521", 1), "multipart-over-limit", access.Token, "FILE_TOO_LARGE", 413, map[string]any{"field": "size_bytes", "max_size_bytes": float64(fixtureLimit)}},
			{strings.Replace(body, "16777217", "16777218", 1), key, access.Token, "IDEMPOTENCY_CONFLICT", 409, nil},
			{body, "multipart-no-auth", "", "AUTH_REQUIRED", 401, nil},
		} {
			encoded, requestID := send(plannedPath, fixture.body, fixture.key, fixture.token, fixture.status)
			var failure dto.ErrorResponse
			if json.Unmarshal(encoded, &failure) != nil || failure.RequestID != requestID || failure.Error.Code != fixture.code ||
				!reflect.DeepEqual(failure.Error.Details, fixture.details) || readState() != state {
				t.Fatalf("公开错误不符或请求改变了数据: %s", encoded)
			}
		}
	})
	t.Run("rejections preserve database facts", func(t *testing.T) {
		before := readState()
		for _, fixture := range []struct {
			body, key   string
			status      int
			code, field string
		}{
			{`{"parent_id":"11","name":"changed.bin","size_bytes":40}`, "create-ordinary", 409, "IDEMPOTENCY_CONFLICT", ""},
			{`{"parent_id":"33","name":"foreign.bin","size_bytes":0}`, "foreign", 404, "ENTRY_NOT_FOUND", ""},
			{`{"parent_id":"15","name":"file.bin","size_bytes":0}`, "file-parent", 400, "INVALID_ARGUMENT", "parent_id"},
			{`{"parent_id":"17","name":"hidden.bin","size_bytes":0}`, "hidden", 404, "ENTRY_NOT_FOUND", ""},
			{`{"parent_id":"11","name":"bad/name","size_bytes":0}`, "bad-name", 400, "INVALID_NAME", "name"},
			{`{"parent_id":"11","name":"big.bin","size_bytes":16777217}`, "oversize", 413, "FILE_TOO_LARGE", "size_bytes"},
			{`{"parent_id":"11","name":"big.bin","size_bytes":18446744073709551615}`, "uint-size", 413, "FILE_TOO_LARGE", "size_bytes"},
			{`{"parent_id":"11","name":"missing.bin"}`, "missing-size", 400, "INVALID_ARGUMENT", "size_bytes"},
			{`{"parent_id":"11","name":"hash.bin","size_bytes":0,"sha256":"private-hash"}`, "bad-sha", 400, "INVALID_ARGUMENT", "sha256"},
			{`{"parent_id":"11","name":"unknown.bin","size_bytes":0,"user_id":99}`, "unknown", 400, "INVALID_ARGUMENT", ""},
			{ordinary, "", 400, "INVALID_ARGUMENT", "Idempotency-Key"},
		} {
			request(fixture.body, fixture.key, access.Token, fixture.status, fixture.code, fixture.field)
			if readState() != before {
				t.Fatal("非法请求产生了写入")
			}
		}
		request(`{`, "", "", 401, "AUTH_REQUIRED", "")
		request(`{`, "", "private-access", 401, "AUTH_REQUIRED", "")
		if readState() != before {
			t.Fatal("未认证请求产生了写入")
		}
	})
	t.Run("quota encoding failure and expired record", func(t *testing.T) {
		before := readState()
		if _, err := pool.ExecContext(ctx, "UPDATE users SET storage_quota=used_storage+reserved_storage WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		request(ordinary, "quota", access.Token, 507, "QUOTA_EXCEEDED", "")
		if _, err := pool.ExecContext(ctx, "UPDATE users SET storage_quota=104857600 WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if readState() != before {
			t.Fatal("配额不足后产生了写入")
		}
		failEncode.Store(true)
		request(ordinary, "encoding-failure", access.Token, 500, "INTERNAL_ERROR", "")
		if readState() != before {
			t.Fatal("编码失败后四项事实未回滚")
		}
		request(ordinary, "expired-result", access.Token, 201, "", "")
		if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET created_at=UTC_TIMESTAMP(6)-INTERVAL 2 DAY,expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 DAY WHERE user_id=1 AND operation=? AND idempotency_key='expired-result'", domain.CreateUploadOperation); err != nil {
			t.Fatal(err)
		}
		before = readState()
		request(ordinary, "expired-result", access.Token, 409, "IDEMPOTENCY_EXPIRED", "")
		if readState() != before {
			t.Fatal("到期结果触发了重新创建")
		}
	})
	t.Run("saved declaration association", func(t *testing.T) {
		request(ordinary, "declaration-guard", access.Token, 201, "", "")
		record, err := store.FindUserIdempotencyRecord(ctx, 1, domain.CreateUploadOperation, "declaration-guard")
		if err != nil {
			t.Fatal(err)
		}
		before := readState()
		for _, mode := range []string{"parent", "name", "size"} {
			if _, err := pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=? WHERE id=?", record.ResponseBody, record.ID); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "parent":
				_, err = pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=JSON_SET(response_body,'$.task.parent_id','33') WHERE id=?", record.ID)
			case "name":
				_, err = pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=JSON_SET(response_body,'$.task.requested_name','wrong.bin') WHERE id=?", record.ID)
			case "size":
				_, err = pool.ExecContext(ctx, "UPDATE idempotency_records SET response_body=JSON_SET(response_body,'$.task.size_bytes',41) WHERE id=?", record.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			request(ordinary, "declaration-guard", access.Token, 500, "INTERNAL_ERROR", "")
			if readState() != before {
				t.Fatal("快照声明不一致时产生了业务写入")
			}
		}
	})
	t.Run("identity rechecked inside transaction", func(t *testing.T) {
		before := readState()
		disableAfterAuthorization.Store(true)
		request(ordinary, "authorization-state-change", access.Token, 403, "ACCOUNT_DISABLED", "")
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		if readState() != before {
			t.Fatal("认证后账号失效仍产生了创建")
		}
	})
	t.Run("full unsigned HTTP identity and target", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash,storage_quota) VALUES(?,'upload_handler_max','完整用户','fixture_hash',?)", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(?,?,'48ee758a-0727-42de-ae7d-3bb33155e148','完整设备','WINDOWS')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(?,?,?,UNHEX(REPEAT('44',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)", maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{"ALTER TABLE stored_objects AUTO_INCREMENT = 9223372036854775810", "ALTER TABLE upload_tasks AUTO_INCREMENT = 9223372036854775810"} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		maxAccess, err := signer.Issue(domain.LoginSession{ID: maximum, UserID: maximum, DeviceID: maximum, ExpiresAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		body := `{"parent_id":"18446744073709551615","name":"maximum.bin","size_bytes":0}`
		first := request(body, "maximum-request", maxAccess.Token, 201, "", "")
		second := request(body, "maximum-request", maxAccess.Token, 201, "", "")
		taskID, err := strconv.ParseUint(first.Data.Task.ID, 10, 64)
		if err != nil || taskID < uint64(1)<<63 || first.Data.Task.ParentID != "18446744073709551615" || !reflect.DeepEqual(first.Data, second.Data) {
			t.Fatal("完整身份、父目录或高位任务标识丢失", err)
		}
	})
	if requestNumber != 37 {
		t.Fatal("实际上传处理请求数量与用例清单不一致", requestNumber)
	}
	lines := bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n"))
	if len(lines) != requestNumber {
		t.Fatal("HTTP 完成日志数量错误")
	}
	for index, line := range lines {
		var completed map[string]any
		if json.Unmarshal(line, &completed) != nil || completed["request_id"] != fmt.Sprintf("28ee758a-0727-42de-ae7d-%012d", index+1) || completed["path"] != requestPaths[index] {
			t.Fatal("HTTP 完成日志关联错误")
		}
	}
	for _, marker := range []string{"private-query", "private-cookie", "private-access", "private-hash", "private-upload-encoding", "学习.bin"} {
		if strings.Contains(logs.String(), marker) {
			t.Fatal("完成日志包含原始请求或业务名称")
		}
	}
}
