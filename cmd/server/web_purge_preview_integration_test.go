package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"golang.org/x/crypto/bcrypt"
)

func TestServerWebPurgePreviewIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_WEB_PURGE_PREVIEW_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_WEB_PURGE_PREVIEW_TEST=1 后在独占库读取清理预检")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool, baseURL := prepareWebPurgePreviewFixture(t, ctx)
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()

	request := func(method, path, token string, body []byte, status int, result any) {
		t.Helper()
		if method != http.MethodGet && (method != http.MethodPost || path != "/api/v1/auth/login") {
			t.Fatal("预检验收仅允许读取和创建隔离会话")
		}
		input, err := http.NewRequestWithContext(ctx, method, baseURL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if method == http.MethodPost {
			input.Header.Set("Origin", baseURL)
			input.Header.Set("Content-Type", "application/json")
			input.Header.Set("X-CloudNest-Client", "web")
		}
		if token != "" {
			input.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(input)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != status {
			t.Fatalf("%s %s 状态 %d，期望 %d", method, path, response.StatusCode, status)
		}
		if err := json.NewDecoder(response.Body).Decode(result); err != nil {
			t.Fatal(err)
		}
	}
	login := func(username string) string {
		t.Helper()
		body, err := json.Marshal(dto.LoginRequest{
			Username: username, Password: webLoginFixturePassword,
			InstallationID: "ba104870-c1d8-42d8-b904-5464baf37b41",
			DeviceName:     "隔离预检", ClientType: "web", Platform: "WEB",
		})
		if err != nil {
			t.Fatal(err)
		}
		var response dto.LoginResponse
		request(http.MethodPost, "/api/v1/auth/login", "", body, http.StatusOK, &response)
		return response.Data.AccessToken
	}

	// 1. 取得两个账号的隔离会话，固定预检前的业务数据。
	ownerToken := login(webLoginFixtureUsername)
	otherToken := login(webEmptyRootFixtureUsername)
	before := readWebPurgePreviewSnapshot(t, ctx, pool)

	// 2. 通过正常入口读取范围，独立回收的后代也计入文件数和逻辑字节数。
	const previewPath = "/api/v1/trash/56/purge-preview"
	var preview dto.PurgePreviewResponse
	request(http.MethodGet, previewPath, ownerToken, nil, http.StatusOK, &preview)
	scope := preview.Data
	if scope.EntryID != "56" || scope.Revision != "1" || scope.FileCount != 2 ||
		scope.DirectoryCount != 2 || scope.LogicalBytes != "4096" {
		t.Fatal("回收目录预检范围不符")
	}

	// 3. 核对匿名、跨账号和不可独立预检的嵌套项，复查状态与引用完全一致。
	for _, test := range []struct {
		path, token string
		status      int
		code        string
	}{
		{previewPath, "", http.StatusUnauthorized, "AUTH_REQUIRED"},
		{previewPath, otherToken, http.StatusNotFound, "ENTRY_NOT_FOUND"},
		{"/api/v1/trash/59/purge-preview", ownerToken, http.StatusNotFound, "ENTRY_NOT_FOUND"},
	} {
		var failure dto.ErrorResponse
		request(http.MethodGet, test.path, test.token, nil, test.status, &failure)
		if failure.Error.Code != test.code {
			t.Fatal("清理预检拒绝原因不符", failure.Error.Code)
		}
	}
	if after := readWebPurgePreviewSnapshot(t, ctx, pool); !slices.Equal(after, before) {
		t.Fatal("只读预检前后的业务状态不一致")
	}
	t.Log("Web 清理预检联测通过：2 个文件、2 个目录、4096 逻辑字节，状态、版本、对象引用、配额和幂等记录一致")
}

func prepareWebPurgePreviewFixture(t *testing.T, ctx context.Context) (*sql.DB, string) {
	t.Helper()
	// 1. 以近期回收的一棵合成子树验收，存储依赖只走本地只读桩。
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	prepareServerReadOnlyStorageStub(t, "fixture-web-purge-preview")
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(webLoginFixturePassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash,storage_quota,used_storage) VALUES(7,?,?,?,104857600,4096),(8,?,?,?,104857600,0)",
		webLoginFixtureUsername, webLoginFixtureDisplayName, string(passwordHash), webEmptyRootFixtureUsername, "隔离预检其他账号", string(passwordHash)); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(33,7,'',X'','DIR'),(63,8,'',X'','DIR')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at) VALUES(41,7,'MINIO','fixture-web-purge-preview','preview-object',2048,'READY',UNHEX(REPEAT('01',32)),UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(56,7,33,'预检回收目录',CAST('预检回收目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(57,7,56,'预检子目录',CAST('预检子目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,status,deleted_at) VALUES(58,7,57,'预检文件.md',CAST('预检文件.md' AS BINARY),'FILE','DOCUMENT',41,2048,'ACTIVE',NULL),(59,7,56,'独立回收.md',CAST('独立回收.md' AS BINARY),'FILE','DOCUMENT',41,2048,'TRASHED',UTC_TIMESTAMP(6))",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}

	// 2. 使用真实 HTTP 装配和本地 Vite，永久清理 worker 在隔离实例中停用。
	baseURL := startServerWebFixture(t, ctx, databaseName, func(ctx context.Context, logger *slog.Logger) error {
		return runWithPurgeRootWorker(ctx, logger,
			func(context.Context, func(context.Context, int, int) (service.PurgeRootBatchResult, error), *slog.Logger) func() {
				return func() {}
			})
	})
	return pool, baseURL
}

func readWebPurgePreviewSnapshot(t *testing.T, ctx context.Context, pool *sql.DB) []string {
	t.Helper()
	// 对业务记录排序，逐项比较状态、位置、版本、对象引用、配额与写入记录。
	rows, err := pool.QueryContext(ctx, `SELECT CAST(JSON_ARRAY('entry',id,user_id,parent_id,name,status,revision,object_id,file_size,deleted_at,purge_started_at,purged_at,updated_at) AS CHAR)
		FROM file_entries
		UNION ALL SELECT CAST(JSON_ARRAY('object',id,status,file_size,unreferenced_at,gc_after,updated_at) AS CHAR) FROM stored_objects
		UNION ALL SELECT CAST(JSON_ARRAY('quota',id,used_storage,reserved_storage) AS CHAR) FROM users
		UNION ALL SELECT CAST(JSON_ARRAY('idempotency_count',COUNT(*)) AS CHAR) FROM idempotency_records
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var snapshot []string
	for rows.Next() {
		var record string
		if err := rows.Scan(&record); err != nil {
			t.Fatal(err)
		}
		snapshot = append(snapshot, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
