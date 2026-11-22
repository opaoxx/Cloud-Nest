package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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

func TestCreateUploadReplacementHandlerIntegration(t *testing.T) {
	// 1. 安装隔离夹具，绑定真实 JWT、账号事务和替换创建服务。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	sessionStart := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	sessionEnd := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	receiptStart := strings.Index(migrations.InitialSQL, "CREATE TABLE idempotency_records (")
	if sessionStart < 0 || sessionEnd <= sessionStart || receiptStart < 0 {
		t.Fatal("正式事务夹具表定义未找到")
	}
	execute := func(statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		migrations.InitialSQL[sessionStart:sessionEnd], migrations.InitialSQL[receiptStart:],
		"UPDATE users SET status='ACTIVE' WHERE id=8",
		"UPDATE users SET used_storage=16777216,reserved_storage=512 WHERE id=7",
		"UPDATE file_entries SET revision=6 WHERE id=31",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(33,7,13,'hidden',X'68696464656e','DIR','TRASHED',UTC_TIMESTAMP(6))",
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision)
			VALUES(34,7,33,'hidden.bin',CAST('hidden.bin' AS BINARY),'FILE','OTHER',29,16777216,9)`,
		`INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES
			(17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR),
			(19,8,12,UNHEX(REPEAT('33',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`,
	} {
		execute(statement)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	configuration := config.JWTConfig{SigningKey: bytes.Repeat([]byte{9}, 32), Issuer: "cloudnest-test", Audience: "replacement-fixture",
		AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: time.Hour}
	signer, err := service.NewAccessTokenSigner(configuration)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewAccessTokenVerifier(configuration)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(userID, deviceID, sessionID uint64) string {
		t.Helper()
		token, err := signer.Issue(domain.LoginSession{ID: sessionID, UserID: userID, DeviceID: deviceID, ExpiresAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		return token.Token
	}
	ownerToken, otherToken := issue(7, 11, 17), issue(8, 12, 19)
	authorization := service.NewAuthorizationService(verifier.Verify, store.FindSessionAuthorization)
	creator := service.NewIdempotentCreateUploadReplacementService("MINIO", "fixture-bucket",
		func(ctx context.Context, actor domain.Actor, action func(context.Context, service.IdempotentCreateUploadReplacementTransaction) error) error {
			return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
		}, dto.EncodeCreateUploadReplacementSnapshot, dto.ValidateCreateUploadReplacementSnapshot)
	const singleLimit = domain.MaxSingleUploadSizeBytes
	const fixtureLimit = 2 * singleLimit
	limits := domain.UploadPlanLimits{MaxFileSizeBytes: fixtureLimit, MaxObjectSizeBytes: fixtureLimit, MaxPartSizeBytes: singleLimit}
	stamp := time.Date(2026, 11, 23, 8, 0, 0, 0, time.UTC)
	emptyDigest := sha256.Sum256(nil)
	wrongBinding, err := dto.EncodeCreateUploadReplacementSnapshot(&domain.UploadTask{
		ID: 90, UserID: 7, DeviceID: 11, StagedObjectID: 23, TargetParentID: 13, RequestedName: "fixture.bin",
		UploadMode: "SINGLE", Status: domain.UploadStatusUploading, ExpectedSHA256: &emptyDigest,
		ReplacementEntryID: new(uint64(32)), ReplacementExpectedRevision: new(uint64(6)),
		CreatedAt: stamp, UpdatedAt: stamp, ExpiresAt: stamp.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. 用本地测试路由连接正式认证和创建处理函数。
	const path = "/test-replacement-create/"
	var serviceCalls atomic.Int32
	router := serverhttp.NewRouter(logging.New(io.Discard), nil, func(context.Context) bool { return true })
	router.POST(path+":id", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure),
		handler.CreateUploadReplacement(func(ctx context.Context, actor domain.Actor, entryID uint64, input dto.CreateUploadReplacementInput, key string) (json.RawMessage, error) {
			serviceCalls.Add(1)
			if key == "fixture-wrong-binding" {
				return wrongBinding, nil
			}
			return creator.Create(ctx, actor, service.UploadReplacementDeclaration{
				EntryID: entryID, ExpectedRevision: input.ExpectedRevision, SizeBytes: input.SizeBytes, ExpectedSHA256: input.ExpectedSHA256,
			}, key, limits)
		}))
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 4 * time.Second
	const fixtureRequestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
	send := func(t *testing.T, id, key, token, body string, wantStatus int) []byte {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path+id, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Request-ID", fixtureRequestID)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		encoded, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != wantStatus || response.Header.Get("X-Request-ID") != fixtureRequestID ||
			response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("替换响应不符，状态 %d，期望 %d，正文 %s，读取错误 %v", response.StatusCode, wantStatus, encoded, err)
		}
		return encoded
	}
	const digest = "15e2b0d3c33891ebb0f1ef609ec419420c20e320ce94c65fbc8c3312448eb225"
	bodyFor := func(revision, size uint64) string {
		return fmt.Sprintf(`{"expected_revision":"%d","size_bytes":%d,"sha256":"%s"}`, revision, size, digest)
	}
	readState := func(t *testing.T) string {
		t.Helper()
		var state string
		if err := pool.QueryRowContext(ctx, `SELECT CONCAT_WS('|',used_storage,reserved_storage,
			(SELECT COUNT(*) FROM upload_tasks),(SELECT COUNT(*) FROM stored_objects),(SELECT COUNT(*) FROM idempotency_records))
			FROM users WHERE id=7`).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}

	// 3. 核对两种模式、同键重试及拒绝时的记录范围。
	for _, size := range []uint64{0, singleLimit + 1} {
		sizeText := strconv.FormatUint(size, 10)
		t.Run("size-"+sizeText, func(t *testing.T) {
			key := "create-" + sizeText
			first := send(t, "31", key, ownerToken, bodyFor(6, size), http.StatusCreated)
			var reply struct {
				Data      json.RawMessage `json:"data"`
				RequestID string          `json:"request_id"`
			}
			if err := json.Unmarshal(first, &reply); err != nil || reply.RequestID != fixtureRequestID {
				t.Fatalf("替换创建响应结构不符 %s，错误 %v", first, err)
			}
			record, err := store.FindUserIdempotencyRecord(ctx, 7, domain.CreateUploadReplacementOperation, key)
			if err != nil || record.ResourceID == nil ||
				dto.ValidateCreateUploadReplacementSnapshot(reply.Data, *record.ResourceID, 31, 6, size) != nil {
				t.Fatalf("已交付快照与回执不一致，错误 %v", err)
			}
			afterFirst := readState(t)
			again := send(t, "31", key, ownerToken, bodyFor(6, size), http.StatusCreated)
			if !bytes.Equal(first, again) || readState(t) != afterFirst {
				t.Fatal("同键 HTTP 重试生成了不同响应或新增数据库记录")
			}
		})
	}
	execute("UPDATE users SET storage_quota=used_storage+reserved_storage WHERE id=7")
	beforeReject := readState(t)
	for _, fixture := range []struct {
		name, id, key, token, body, code string
		status                           int
		details                          map[string]any
		reachesService                   bool
	}{
		{"unauthorized first", "0", "", "", "{", "AUTH_REQUIRED", 401, nil, false},
		{"path before key", "0", "", ownerToken, "{", "INVALID_ARGUMENT", 400, map[string]any{"field": "id"}, false},
		{"key before body", "31", "", ownerToken, "{", "INVALID_ARGUMENT", 400, map[string]any{"field": "Idempotency-Key"}, false},
		{"malformed body", "31", "reject-json", ownerToken, "{", "INVALID_ARGUMENT", 400, nil, false},
		{"missing digest", "31", "reject-sha", ownerToken, `{"expected_revision":"6","size_bytes":0}`, "INVALID_ARGUMENT", 400, map[string]any{"field": "sha256"}, false},
		{"directory target", "13", "reject-dir", ownerToken, bodyFor(1, 0), "INVALID_ARGUMENT", 400, map[string]any{"field": "id"}, true},
		{"hidden target", "34", "reject-hidden", ownerToken, bodyFor(1, 0), "ENTRY_NOT_FOUND", 404, nil, true},
		{"foreign target", "31", "create-0", otherToken, bodyFor(6, 0), "ENTRY_NOT_FOUND", 404, nil, true},
		{"old revision", "31", "reject-revision", ownerToken, bodyFor(5, 0), "REVISION_CONFLICT", 409, map[string]any{"current_revision": "6"}, true},
		{"changed request", "31", "create-0", ownerToken, bodyFor(6, 1), "IDEMPOTENCY_CONFLICT", 409, nil, true},
		{"limit before quota", "31", "reject-size", ownerToken, bodyFor(6, fixtureLimit+1), "FILE_TOO_LARGE", 413,
			map[string]any{"field": "size_bytes", "max_size_bytes": float64(fixtureLimit)}, true},
		{"quota exhausted", "31", "reject-quota", ownerToken, bodyFor(6, 1), "QUOTA_EXCEEDED", 507, nil, true},
		{"mismatched success binding", "31", "fixture-wrong-binding", ownerToken, bodyFor(6, 0), "INTERNAL_ERROR", 500, nil, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			beforeCalls := serviceCalls.Load()
			encoded := send(t, fixture.id, fixture.key, fixture.token, fixture.body, fixture.status)
			var reply dto.ErrorResponse
			if err := json.Unmarshal(encoded, &reply); err != nil || reply.Error.Code != fixture.code || reply.RequestID != fixtureRequestID ||
				!reflect.DeepEqual(reply.Error.Details, fixture.details) || strings.Contains(string(encoded), digest) {
				t.Fatalf("公开错误不符 %s，错误 %v", encoded, err)
			}
			expectedCalls := beforeCalls
			if fixture.reachesService {
				expectedCalls++
			}
			if serviceCalls.Load() != expectedCalls || readState(t) != beforeReject {
				t.Fatal("请求校验顺序错误或拒绝后新增数据库记录")
			}
		})
	}
}
