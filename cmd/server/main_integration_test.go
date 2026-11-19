package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/config"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/security"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
	"golang.org/x/crypto/bcrypt"
)

func TestServerCurrentUserIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_SERVER_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_SERVER_TEST=1 后使用独占库、临时用户配置目录和现有 MinIO 验证真实启动")
	}
	// 1. 创建独占数据库，使用固定正式迁移建立启动检查及用户夹具。
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	databaseConfig, err := config.LoadDatabaseConfig()
	if err != nil {
		t.Fatal(err)
	}
	admin, err := repository.OpenMySQL(ctx, databaseConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	const prefix = "cloudnest_server_verify_"
	databaseName := prefix + strconv.FormatInt(time.Now().UnixNano(), 10)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+databaseName+"`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !strings.HasPrefix(databaseName, prefix) {
			t.Error("拒绝清理非本用例数据库")
			return
		}
		if _, err := strconv.ParseUint(strings.TrimPrefix(databaseName, prefix), 10, 64); err != nil {
			t.Error("独占库名不符合清理规则")
			return
		}
		cleanupContext, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := admin.ExecContext(cleanupContext, "DROP DATABASE `"+databaseName+"`"); err != nil {
			t.Error("独占启动测试库清理失败")
			return
		}
		var remaining int
		if err := admin.QueryRowContext(cleanupContext, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name=?", databaseName).Scan(&remaining); err != nil || remaining != 0 {
			t.Error("独占启动测试库清理复核失败")
		}
	})
	driverConfig := mysql.NewConfig()
	driverConfig.Net, driverConfig.Addr, driverConfig.DBName = "tcp", databaseConfig.Address, databaseName
	driverConfig.User, driverConfig.Passwd = databaseConfig.Username, databaseConfig.Password
	driverConfig.ParseTime, driverConfig.MultiStatements = true, true
	driverConfig.Timeout, driverConfig.ReadTimeout, driverConfig.WriteTimeout = 5*time.Second, 10*time.Second, 10*time.Second
	driverConfig.Params = map[string]string{"time_zone": "'+00:00'"}
	if err := driverConfig.Apply(mysql.Charset("utf8mb4", "")); err != nil {
		t.Fatal(err)
	}
	connector, err := mysql.NewConnector(driverConfig)
	if err != nil {
		t.Fatal(err)
	}
	pool := sql.OpenDB(connector)
	t.Cleanup(func() { _ = pool.Close() })
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "CREATE TABLE schema_migrations(version BIGINT UNSIGNED PRIMARY KEY,checksum CHAR(64) NOT NULL,dirty BOOLEAN NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(migrations.InitialSQL)))
	if _, err := pool.ExecContext(ctx, "INSERT INTO schema_migrations VALUES(1,?,false)", checksum); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,storage_quota,used_storage,reserved_storage) VALUES(1,'fixture_one','真实启动用户','fixture_hash',100,60,40)",
		"INSERT INTO users(id,username,display_name,password_hash,storage_quota) VALUES(3,'fixture_other','其他启动用户','fixture_hash',100)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-server-directory',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(33,3,NULL,'',X'','DIR'),(14,1,11,'祖先目录',CAST('祖先目录' AS BINARY),'DIR'),(12,1,14,'当前目录',CAST('当前目录' AS BINARY),'DIR'),(15,1,11,'空目录',CAST('空目录' AS BINARY),'DIR'),(21,1,12,'子项一',CAST('子项一' AS BINARY),'DIR'),(22,1,12,'子项二',CAST('子项二' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(23,1,12,'学习笔记.md',CAST('学习笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048,3),(42,1,11,'根下文件.md',CAST('根下文件.md' AS BINARY),'FILE','DOCUMENT',41,2048,1)",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','启动测试设备','WEB')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(23,3,'0adfc616-05b0-4469-a138-806e7aa667f8','其他账号设备','WINDOWS')",
		"UPDATE file_entries SET color_key='sand' WHERE id=12",
		"UPDATE file_entries SET extension='md',mime_type='text/markdown',source_device_id=13,source_device_name='发布时设备 🎮',content_modified_at='2026-11-15 02:30:00.123456' WHERE id=23",
		"INSERT INTO user_entry_preferences(user_id,entry_id,starred_at,last_opened_at) VALUES(1,23,'2026-11-15 02:30:00.123456','2026-11-15 02:30:01.123456')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	directoryStamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=?", directoryStamp); err != nil {
		t.Fatal(err)
	}
	passwordHash, err := bcrypt.GenerateFromPassword([]byte("fixture-password"), 12)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE users SET password_hash=? WHERE id=1", string(passwordHash)); err != nil {
		t.Fatal(err)
	}
	session := domain.LoginSession{ID: 17, UserID: 1, DeviceID: 13, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
	hash := sha256.Sum256([]byte("fixture-refresh"))
	if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,?,?)", hash[:], session.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	foreignSession := domain.LoginSession{ID: 27, UserID: 3, DeviceID: 23, ExpiresAt: session.ExpiresAt}
	foreignHash := sha256.Sum256([]byte("fixture-other-refresh"))
	if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(27,3,23,?,?)", foreignHash[:], foreignSession.ExpiresAt); err != nil {
		t.Fatal(err)
	}

	// 2. 为本用例设置临时配置目录和空闲端口，真正执行生产 run 启动流程。
	configurationDirectory := t.TempDir()
	configVariable := "XDG_CONFIG_HOME"
	switch runtime.GOOS {
	case "windows":
		configVariable = "APPDATA"
	case "darwin", "ios":
		configVariable = "HOME"
	case "plan9":
		configVariable = "home"
	}
	t.Setenv(configVariable, configurationDirectory)
	configurationDirectory, err = os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLOUDNEST_JWT_SECRET", "CLOUDNEST_JWT_ISSUER", "CLOUDNEST_JWT_AUDIENCE", "ACCESS_TOKEN_TTL", "REFRESH_TOKEN_TTL"} {
		t.Setenv(name, "fixture")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLOUDNEST_DB_NAME", databaseName)
	t.Setenv("CLOUDNEST_COOKIE_SECURE", "false")
	uploadDirectory := filepath.Join(t.TempDir(), "上传 临时目录")
	t.Setenv("CLOUDNEST_TEMP_DIR", uploadDirectory)
	var uploadStorage *serverUploadContentBucket
	if os.Getenv("CLOUDNEST_UPLOAD_STORAGE_TEST") == "1" {
		uploadStorage = prepareServerUploadContentBucket(t, ctx)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDNEST_HTTP_ADDR", address)
	var logs bytes.Buffer
	logger := logging.New(&logs)
	client := &http.Client{Timeout: 2 * time.Second}
	start := func() func() {
		runContext, stop := context.WithCancel(ctx)
		result := make(chan error, 1)
		go func() { result <- run(runContext, logger) }()
		var once sync.Once
		shutdown := func() {
			once.Do(func() {
				stop()
				select {
				case err := <-result:
					if err != nil {
						t.Error("真实服务未正常关闭")
					}
				case <-time.After(35 * time.Second):
					t.Error("真实服务关闭超过等待期限")
				}
			})
		}
		t.Cleanup(shutdown)
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			response, err := client.Get("http://" + address + "/health/live")
			if err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode == 200 {
					return shutdown
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("真实启动未在期限内提供 HTTP 服务")
		return shutdown
	}
	stopFirst := start()
	uploadEntries, err := os.ReadDir(uploadDirectory)
	if err != nil || len(uploadEntries) != 0 {
		t.Fatal("启动 HTTP 前未完成临时目录初始化和探测清理", err)
	}
	uploadNeighbor := filepath.Join(uploadDirectory, "startup-keep.txt")
	if err := os.WriteFile(uploadNeighbor, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	request := func(t *testing.T, path, tokenText string, expectedStatus int) []byte {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, "http://"+address+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tokenText != "" {
			request.Header.Set("Authorization", "Bearer "+tokenText)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("正式路由请求失败")
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != expectedStatus {
			t.Fatal("正式路由响应状态错误")
		}
		return body
	}
	for _, path := range []string{"/ping", "/health/live", "/health/ready"} {
		request(t, path, "", 200)
	}
	request(t, "/api/v1/me", "", 401)
	jwtConfig, err := config.LoadServerJWTConfig()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := service.NewAccessTokenSigner(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewAccessTokenVerifier(jwtConfig)
	if err != nil {
		t.Fatal(err)
	}
	foreignToken, err := signer.Issue(foreignSession)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Issue(session)
	if err != nil {
		t.Fatal("正式路由测试令牌签发失败")
	}
	checkProfile := func(t *testing.T) {
		t.Helper()
		body := request(t, "/api/v1/me?user_id=99", token.Token, 200)
		var response dto.UserProfileResponse
		if err := json.Unmarshal(body, &response); err != nil || response.Data.User.ID != "1" || response.Data.User.DisplayName != "真实启动用户" || response.Data.RootEntryID != "11" || response.Data.QuotaBytes != 100 || response.Data.UsedBytes != 60 || response.Data.ReservedBytes != 40 || response.RequestID == "" {
			t.Fatal("真实启动返回的数据库资料错误")
		}
	}
	checkProfile(t)
	// 正式登录接口从密码验证开始，创建的令牌再访问当前用户接口。
	loginSecrets := []string{foreignToken.Token, "fixture-other-refresh"}
	// 正式详情接口逐项核对公开字段与当前用户范围内的数据库事实。
	verificationStore, err := repository.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	getEntryDetail := func(t *testing.T, path, accessToken string, status int, code, field string) dto.EntryDetailResponse {
		t.Helper()
		// 1. 访问正式路径，核对网络交付、请求关联、缓存及错误结构。
		detailRequest, err := http.NewRequest(http.MethodGet, "http://"+address+"/api/v1/entries/"+path, strings.NewReader(`{"id":"99","user_id":"99"}`))
		if err != nil {
			t.Fatal(err)
		}
		detailRequest.URL.RawQuery = "id=99&user_id=99&note=private-detail-query"
		detailRequest.Header.Set("Origin", "http://127.0.0.1:5173")
		detailRequest.Header.Set("X-User-ID", "99")
		if accessToken != "" {
			detailRequest.Header.Set("Authorization", "Bearer "+accessToken)
		}
		response, err := client.Do(detailRequest)
		if err != nil {
			t.Fatal("正式详情网络请求失败", err)
		}
		defer response.Body.Close()
		encoded, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Request-ID") == "" ||
			response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || response.Header.Get("Content-Type") != "application/json; charset=utf-8" ||
			response.Header.Get("Content-Encoding") != "" || len(response.Cookies()) != 0 || ((status == 401) != (response.Header.Get("WWW-Authenticate") != "")) {
			t.Fatal("正式详情状态、关联或响应头错误", response.StatusCode, err)
		}
		var payload dto.EntryDetailResponse
		if status != 200 {
			var failure dto.ErrorResponse
			if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("正式详情错误结构异常")
			}
			if field == "" {
				if failure.Error.Details != nil {
					t.Fatal("详情非字段错误 details 应为空")
				}
			} else if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
				t.Fatal("正式详情错误字段不正确")
			}
			if code == "ENTRY_NOT_FOUND" && failure.Error.Message != "目录项不存在" {
				t.Fatal("正式详情目标不存在提示错误")
			}
			return payload
		}
		if json.Unmarshal(encoded, &payload) != nil || payload.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Pragma") != "no-cache" || response.ContentLength != int64(len(encoded)) {
			t.Fatal("正式详情成功结构或长度错误")
		}
		var outer map[string]json.RawMessage
		var fields map[string]json.RawMessage
		if json.Unmarshal(encoded, &outer) != nil || len(outer) != 2 || json.Unmarshal(outer["data"], &fields) != nil || len(fields) != 18 {
			t.Fatal("正式详情公开字段数量错误")
		}

		// 2. 根据已校验令牌取得用户，独立读取三类 Repository 事实。
		identity, err := verifier.Verify(accessToken)
		if err != nil || identity == nil {
			t.Fatal("核对正式详情身份失败")
		}
		targetID, err := strconv.ParseUint(strings.TrimPrefix(detailRequest.URL.Path, "/api/v1/entries/"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		target, err := verificationStore.FindUserFileEntry(ctx, identity.UserID, targetID)
		if err != nil {
			t.Fatal("正式详情目标归属与数据库不符", err)
		}
		display, err := verificationStore.FindUserFileEntryDisplay(ctx, identity.UserID, targetID)
		if err != nil {
			t.Fatal(err)
		}
		preference, err := verificationStore.FindUserEntryPreference(ctx, identity.UserID, targetID)
		if err != nil {
			t.Fatal(err)
		}
		expected := dto.EntryDetailData{
			ID: strconv.FormatUint(target.ID, 10), Name: target.Name, Kind: "folder", SizeBytes: target.FileSize,
			Revision: strconv.FormatUint(target.Revision, 10), Status: target.Status,
			CreatedAt: target.CreatedAt, ModifiedAt: target.ModifiedAt, UpdatedAt: target.UpdatedAt,
			Extension: display.Extension, MIMEType: display.MIMEType, ColorKey: display.ColorKey, ContentModifiedAt: display.ContentModifiedAt,
			Starred: preference.StarredAt != nil, StarredAt: preference.StarredAt, LastOpenedAt: preference.LastOpenedAt,
		}
		if target.ParentID != nil {
			parentID := strconv.FormatUint(*target.ParentID, 10)
			expected.ParentID = &parentID
		}
		if target.EntryType == "FILE" {
			if display.Category == nil {
				t.Fatal("正式文件详情缺少数据库类别")
			}
			expected.Kind = strings.ToLower(*display.Category)
		}
		if display.SourceDeviceID != nil || display.SourceDeviceName != nil {
			expected.SourceDevice = &dto.EntrySourceDeviceSummary{Name: display.SourceDeviceName}
			if display.SourceDeviceID != nil {
				deviceID := strconv.FormatUint(*display.SourceDeviceID, 10)
				expected.SourceDevice.ID = &deviceID
			}
		}

		// 3. 核对十八项公开字段以及必填和可空时间的 UTC 微秒格式。
		if !reflect.DeepEqual(payload.Data, expected) {
			t.Fatal("正式详情字段与数据库事实不一致")
		}
		for _, stamp := range []*time.Time{&payload.Data.CreatedAt, &payload.Data.ModifiedAt, &payload.Data.UpdatedAt, payload.Data.ContentModifiedAt, payload.Data.StarredAt, payload.Data.LastOpenedAt} {
			if stamp != nil && (stamp.Location() != time.UTC || stamp.Nanosecond()%1000 != 0) {
				t.Fatal("正式详情时间应为 UTC 微秒值")
			}
		}
		return payload
	}
	// 正式目录接口按认证身份读取元数据，并独立核对数据库事实与实际配置签名。
	directoryCursorVerifier, err := service.NewDirectoryCursorVerifier(jwtConfig.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	listDirectory := func(t *testing.T, query, accessToken string, status int, code, field string) dto.DirectoryPageResponse {
		t.Helper()
		// 1. 请求正式路径，检查网络交付、请求关联、CORS 和缓存。
		directoryRequest, err := http.NewRequest(http.MethodGet, "http://"+address+"/api/v1/entries", nil)
		if err != nil {
			t.Fatal(err)
		}
		directoryRequest.URL.RawQuery = query
		directoryRequest.Header.Set("Origin", "http://127.0.0.1:5173")
		if accessToken != "" {
			directoryRequest.Header.Set("Authorization", "Bearer "+accessToken)
		}
		response, err := client.Do(directoryRequest)
		if err != nil {
			t.Fatal("正式目录列表网络请求失败", err)
		}
		defer response.Body.Close()
		encoded, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" ||
			response.Header.Get("X-Request-ID") == "" || len(response.Cookies()) != 0 ||
			response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" ||
			response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Content-Encoding") != "" {
			t.Fatal("正式目录列表状态、关联、CORS 或正文头错误", response.StatusCode, err)
		}
		var payload dto.DirectoryPageResponse
		if status != 200 {
			var failure dto.ErrorResponse
			if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("正式目录列表错误响应异常")
			}
			if field == "" {
				if failure.Error.Details != nil {
					t.Fatal("非字段错误 details 应为空")
				}
			} else if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
				t.Fatal("正式目录列表错误字段异常")
			}
			return payload
		}
		if json.Unmarshal(encoded, &payload) != nil || payload.RequestID != response.Header.Get("X-Request-ID") ||
			payload.Data.Items == nil || len(payload.Data.Breadcrumbs) == 0 || response.Header.Get("Pragma") != "no-cache" {
			t.Fatal("正式目录列表成功响应异常")
		}

		// 2. 使用真实令牌确定用户，逐项检查公开元数据及根到当前目录的父链。
		identity, err := verifier.Verify(accessToken)
		if err != nil || identity == nil {
			t.Fatal("核对正式目录列表身份失败")
		}
		parsed, err := url.ParseQuery(query)
		if err != nil {
			t.Fatal(err)
		}
		parentID := parsed.Get("parent_id")
		for _, item := range payload.Data.Items {
			var name, entryType, entryStatus string
			var parent, revision, size uint64
			var createdAt, modifiedAt, updatedAt time.Time
			if err := pool.QueryRowContext(ctx, "SELECT parent_id,name,entry_type,file_size,revision,status,created_at,modified_at,updated_at FROM file_entries WHERE user_id=? AND id=?", identity.UserID, item.ID).
				Scan(&parent, &name, &entryType, &size, &revision, &entryStatus, &createdAt, &modifiedAt, &updatedAt); err != nil {
				t.Fatal("正式目录列表包含归属不符的子项", err)
			}
			if item.ParentID != parentID || item.ParentID != strconv.FormatUint(parent, 10) || item.Name != name || item.EntryType != entryType ||
				item.SizeBytes != size || item.Revision != strconv.FormatUint(revision, 10) || item.Status != "ACTIVE" || item.Status != entryStatus ||
				!item.CreatedAt.Equal(createdAt) || !item.ModifiedAt.Equal(modifiedAt) || !item.UpdatedAt.Equal(updatedAt) {
				t.Fatal("正式目录列表公开字段与数据库事实不一致")
			}
			for _, stamp := range []time.Time{item.CreatedAt, item.ModifiedAt, item.UpdatedAt} {
				if stamp.Location() != time.UTC || stamp.Nanosecond()%1000 != 0 {
					t.Fatal("正式目录列表时间应为 UTC 微秒值")
				}
			}
		}
		for index, crumb := range payload.Data.Breadcrumbs {
			var name string
			var parent sql.NullString
			if err := pool.QueryRowContext(ctx, "SELECT parent_id,name FROM file_entries WHERE user_id=? AND id=? AND entry_type='DIR' AND status='ACTIVE'", identity.UserID, crumb.ID).Scan(&parent, &name); err != nil {
				t.Fatal("正式目录面包屑包含不可见或跨用户目录", err)
			}
			if crumb.Name != name || (index == 0 && (parent.Valid || name != "")) ||
				(index > 0 && (!parent.Valid || parent.String != payload.Data.Breadcrumbs[index-1].ID)) {
				t.Fatal("正式目录面包屑顺序或名称错误")
			}
		}
		if payload.Data.Breadcrumbs[len(payload.Data.Breadcrumbs)-1].ID != parentID {
			t.Fatal("正式目录面包屑终点错误")
		}

		// 3. 使用本次启动配置校验返回游标，确认绑定用户、目录及页尾位置。
		if payload.Data.NextCursor != nil {
			if len(payload.Data.Items) == 0 {
				t.Fatal("空页不应生成后续游标")
			}
			parentNumber, err := strconv.ParseUint(parentID, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			position, err := directoryCursorVerifier.Verify(identity.UserID, parentNumber, *payload.Data.NextCursor)
			last := payload.Data.Items[len(payload.Data.Items)-1]
			if err != nil || position == nil || strconv.FormatUint(position.ID, 10) != last.ID || !position.ModifiedAt.Equal(last.ModifiedAt) {
				t.Fatal("正式目录游标与持久化配置或页尾位置不一致")
			}
			loginSecrets = append(loginSecrets, *payload.Data.NextCursor)
		}
		return payload
	}
	listDirectory(t, "parent_id=0", "", 401, "AUTH_REQUIRED", "")
	for _, failure := range []struct {
		query, code, field string
		status             int
	}{
		{query: "", status: 400, code: "INVALID_ARGUMENT", field: "parent_id"},
		{query: "parent_id=12&limit=0", status: 400, code: "INVALID_ARGUMENT", field: "limit"},
		{query: "parent_id=12&cursor=private-directory-cursor", status: 400, code: "INVALID_CURSOR", field: "cursor"},
		{query: "parent_id=12&scope=unknown", status: 400, code: "INVALID_ARGUMENT", field: "scope"},
		{query: "parent_id=12&sort=name_asc", status: 400, code: "INVALID_ARGUMENT", field: "sort"},
		{query: "parent_id=12&q=private-directory-query", status: 400, code: "INVALID_ARGUMENT", field: "q"},
		{query: "parent_id=12&kind=FILE", status: 400, code: "INVALID_ARGUMENT", field: "kind"},
		{query: "parent_id=33", status: 404, code: "ENTRY_NOT_FOUND"},
		{query: "parent_id=18446744073709551615", status: 404, code: "ENTRY_NOT_FOUND"},
		{query: "parent_id=42", status: 400, code: "INVALID_ARGUMENT", field: "parent_id"},
	} {
		listDirectory(t, failure.query, token.Token, failure.status, failure.code, failure.field)
	}
	directoryFirst := listDirectory(t, "parent_id=12&limit=2&user_id=3&extra=private-directory-query", token.Token, 200, "", "")
	if len(directoryFirst.Data.Items) != 2 || directoryFirst.Data.Items[0].ID != "23" || directoryFirst.Data.Items[1].ID != "22" ||
		directoryFirst.Data.NextCursor == nil || len(directoryFirst.Data.Breadcrumbs) != 3 || directoryFirst.Data.Breadcrumbs[0].ID != "11" || directoryFirst.Data.Breadcrumbs[1].ID != "14" {
		t.Fatal("正式目录首页、同时间倒序或三层路径错误")
	}
	directoryNextQuery := "parent_id=12&limit=2&cursor=" + url.QueryEscape(*directoryFirst.Data.NextCursor)
	directoryLast := listDirectory(t, directoryNextQuery, token.Token, 200, "", "")
	if len(directoryLast.Data.Items) != 1 || directoryLast.Data.Items[0].ID != "21" || directoryLast.Data.NextCursor != nil {
		t.Fatal("正式目录后续页或末页错误")
	}
	rootDirectory := listDirectory(t, "parent_id=11", token.Token, 200, "", "")
	if len(rootDirectory.Data.Items) != 3 || len(rootDirectory.Data.Breadcrumbs) != 1 || rootDirectory.Data.NextCursor != nil {
		t.Fatal("正式根目录列表错误")
	}
	emptyDirectory := listDirectory(t, "parent_id=15&cursor=", token.Token, 200, "", "")
	foreignDirectory := listDirectory(t, "parent_id=33&user_id=1", foreignToken.Token, 200, "", "")
	if len(emptyDirectory.Data.Items) != 0 || emptyDirectory.Data.NextCursor != nil || len(foreignDirectory.Data.Items) != 0 || foreignDirectory.Data.NextCursor != nil {
		t.Fatal("正式空目录或其他账号目录隔离错误")
	}
	listDirectory(t, directoryNextQuery, foreignToken.Token, 400, "INVALID_CURSOR", "cursor")
	listDirectory(t, "parent_id=11&cursor="+url.QueryEscape(*directoryFirst.Data.NextCursor), token.Token, 400, "INVALID_CURSOR", "cursor")
	for _, entryID := range []uint64{12, 14} {
		for _, state := range []string{"TRASHED", "PURGING", "PURGED"} {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,deleted_at=UTC_TIMESTAMP(6),purge_started_at=IF(?='PURGING',UTC_TIMESTAMP(6),NULL),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL) WHERE id=?", state, state, state, entryID); err != nil {
				t.Fatal(err)
			}
			listDirectory(t, directoryNextQuery, token.Token, 404, "ENTRY_NOT_FOUND", "")
			getEntryDetail(t, "23", token.Token, 404, "ENTRY_NOT_FOUND", "")
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL,purge_started_at=NULL,purged_at=NULL WHERE id=?", entryID); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 正式详情读取与目录列表共用元数据，验证路径、根目录及目标边界。
	detailFirst := getEntryDetail(t, "23", token.Token, 200, "", "")
	if detailFirst.Data.Kind != "document" || detailFirst.Data.SourceDevice == nil || *detailFirst.Data.SourceDevice.ID != "13" ||
		*detailFirst.Data.SourceDevice.Name != "发布时设备 🎮" || !detailFirst.Data.Starred || detailFirst.Data.ContentModifiedAt == nil {
		t.Fatal("正式文件详情展示或偏好未装配")
	}
	getEntryDetail(t, "%32%33", token.Token, 200, "", "")
	detailDirectory := getEntryDetail(t, "12", token.Token, 200, "", "")
	detailRoot := getEntryDetail(t, "11", token.Token, 200, "", "")
	if detailDirectory.Data.Kind != "folder" || detailDirectory.Data.ColorKey == nil || *detailDirectory.Data.ColorKey != "sand" ||
		detailRoot.Data.ParentID != nil || detailRoot.Data.Name != "" || detailRoot.Data.Starred {
		t.Fatal("正式目录颜色或根详情错误")
	}
	getEntryDetail(t, "33", foreignToken.Token, 200, "", "")
	getEntryDetail(t, "0", "", 401, "AUTH_REQUIRED", "")
	for _, path := range []string{"0", "023", "18446744073709551616", "%2532%2533"} {
		getEntryDetail(t, path, token.Token, 400, "INVALID_ARGUMENT", "id")
	}
	for _, path := range []string{"33", "999", "18446744073709551615"} {
		getEntryDetail(t, path, token.Token, 404, "ENTRY_NOT_FOUND", "")
	}
	getEntryDetail(t, "23", foreignToken.Token, 404, "ENTRY_NOT_FOUND", "")
	for _, state := range []string{"TRASHED", "PURGING", "PURGED"} {
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status=?,object_id=IF(?='PURGED',NULL,41),deleted_at=UTC_TIMESTAMP(6),purge_started_at=UTC_TIMESTAMP(6),purged_at=IF(?='PURGED',UTC_TIMESTAMP(6),NULL) WHERE id=23", state, state, state); err != nil {
			t.Fatal(err)
		}
		getEntryDetail(t, "23", token.Token, 404, "ENTRY_NOT_FOUND", "")
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',object_id=41,deleted_at=NULL,purge_started_at=NULL,purged_at=NULL WHERE id=23"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET file_size=9007199254740992 WHERE id=23"); err != nil {
		t.Fatal(err)
	}
	getEntryDetail(t, "23", token.Token, 500, "INTERNAL_ERROR", "")
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET file_size=2048 WHERE id=23"); err != nil {
		t.Fatal(err)
	}
	expectedSecureCookie := false
	login := func(t *testing.T, clientType, installationID, password string, status int) (dto.LoginResponse, []*http.Cookie) {
		t.Helper()
		platform := "WEB"
		if clientType == "native" {
			platform = "ANDROID"
		}
		input := dto.LoginRequest{Username: "fixture_one", Password: password, InstallationID: installationID, DeviceName: "集成登录设备", Platform: platform, ClientType: clientType}
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		loginRequest, err := http.NewRequest(http.MethodPost, "http://"+address+"/api/v1/auth/login", bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		loginRequest.Header.Set("Content-Type", "application/json")
		loginRequest.Header.Set("X-CloudNest-Client", clientType)
		if clientType == "web" {
			loginRequest.Header.Set("Origin", "http://127.0.0.1:5173")
		}
		response, err := client.Do(loginRequest)
		if err != nil {
			t.Fatal("正式登录网络请求失败")
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("正式登录响应状态或缓存错误")
		}
		var payload dto.LoginResponse
		if status != 200 {
			var failure dto.ErrorResponse
			if json.Unmarshal(body, &failure) != nil || len(response.Cookies()) != 0 {
				t.Fatal("错误登录响应异常")
			}
			return payload, nil
		}
		if json.Unmarshal(body, &payload) != nil || payload.Data.User.ID != "1" || payload.Data.RootEntryID != "11" || payload.RequestID != response.Header.Get("X-Request-ID") {
			t.Fatal("正式登录资料不一致")
		}
		cookies := response.Cookies()
		refresh := payload.Data.RefreshToken
		if clientType == "web" {
			if refresh != "" || len(cookies) != 1 || cookies[0].Secure != expectedSecureCookie || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/api/v1/auth" {
				t.Fatal("正式 Web 登录 Cookie 属性错误")
			}
			refresh = cookies[0].Value
		} else if refresh == "" || len(cookies) != 0 {
			t.Fatal("正式原生登录刷新原文交付错误")
		}
		var storedHash []byte
		if err := pool.QueryRowContext(ctx, "SELECT refresh_token_hash FROM auth_sessions WHERE user_id=1 AND id=?", payload.Data.Session.ID).Scan(&storedHash); err != nil {
			t.Fatal("登录会话未持久化")
		}
		expected := sha256.Sum256([]byte(refresh))
		if !bytes.Equal(storedHash, expected[:]) {
			t.Fatal("登录返回的刷新原文与入库摘要不一致")
		}
		loginSecrets = append(loginSecrets, payload.Data.AccessToken, refresh)
		var me dto.UserProfileResponse
		if json.Unmarshal(request(t, "/api/v1/me", payload.Data.AccessToken, 200), &me) != nil || me.Data.User.ID != "1" || me.Data.RootEntryID != "11" {
			t.Fatal("登录令牌无法访问当前用户资料")
		}
		return payload, cookies
	}
	// 正式刷新接口使用登录所得原文，核对摘要、代次及新 JWT 的 /me 授权。
	refresh := func(t *testing.T, clientType, rawToken, sessionID string, status int, generation uint64) (dto.RefreshResponse, string) {
		t.Helper()
		input := dto.RefreshRequest{ClientType: clientType}
		if clientType == "native" {
			input.RefreshToken = rawToken
		}
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		refreshRequest, err := http.NewRequest(http.MethodPost, "http://"+address+"/api/v1/auth/refresh", bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		refreshRequest.Header.Set("Content-Type", "application/json")
		refreshRequest.Header.Set(security.ClientHeader, clientType)
		if clientType == "web" {
			refreshRequest.Header.Set("Origin", "http://127.0.0.1:5173")
			refreshRequest.AddCookie(&http.Cookie{Name: security.RefreshCookieName, Value: rawToken})
		}
		response, err := client.Do(refreshRequest)
		if err != nil {
			t.Fatal("正式刷新网络请求失败", err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" {
			t.Fatal("正式刷新状态或缓存头错误", response.StatusCode, err)
		}
		var payload dto.RefreshResponse
		if status != 200 {
			var rejected dto.ErrorResponse
			if json.Unmarshal(body, &rejected) != nil || rejected.Error.Code == "" || rejected.Error.Details != nil || rejected.RequestID != response.Header.Get("X-Request-ID") || len(response.Cookies()) != 0 {
				t.Fatal("正式刷新错误结构异常")
			}
			return payload, ""
		}
		if json.Unmarshal(body, &payload) != nil || payload.Data.AccessToken == "" || payload.Data.ExpiresIn < 0 || payload.RequestID == "" || payload.RequestID != response.Header.Get("X-Request-ID") {
			t.Fatal("正式刷新成功结构异常")
		}
		newRaw := payload.Data.RefreshToken
		cookies := response.Cookies()
		if clientType == "web" {
			if newRaw != "" || len(cookies) != 1 || cookies[0].Name != security.RefreshCookieName || cookies[0].Secure != expectedSecureCookie || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/api/v1/auth" {
				t.Fatal("正式刷新 Cookie 属性错误")
			}
			newRaw = cookies[0].Value
		} else if newRaw == "" || len(cookies) != 0 {
			t.Fatal("正式原生刷新交付异常")
		}
		var storedHash []byte
		var storedGeneration uint64
		var expiresAt time.Time
		if err := pool.QueryRowContext(ctx, "SELECT refresh_token_hash,token_generation,expires_at FROM auth_sessions WHERE user_id=1 AND id=?", sessionID).Scan(&storedHash, &storedGeneration, &expiresAt); err != nil {
			t.Fatal(err)
		}
		expectedHash := sha256.Sum256([]byte(newRaw))
		if !bytes.Equal(storedHash, expectedHash[:]) || storedGeneration != generation {
			t.Fatal("正式刷新摘要或代次不一致")
		}
		if clientType == "web" && !cookies[0].Expires.Equal(expiresAt.UTC().Truncate(time.Second)) {
			t.Fatal("正式刷新 Cookie 期限错误")
		}
		var profile dto.UserProfileResponse
		if json.Unmarshal(request(t, "/api/v1/me", payload.Data.AccessToken, 200), &profile) != nil || profile.Data.User.ID != "1" || profile.Data.RootEntryID != "11" {
			t.Fatal("正式刷新 JWT 无法访问当前用户资料")
		}
		loginSecrets = append(loginSecrets, payload.Data.AccessToken, newRaw)
		return payload, newRaw
	}
	// 正式退出接口使用访问令牌定位会话，确认撤销提交及客户端交付。
	logout := func(t *testing.T, clientType, accessToken, sessionID string, status int, code string) {
		t.Helper()
		encoded, err := json.Marshal(dto.LogoutRequest{ClientType: clientType})
		if err != nil {
			t.Fatal(err)
		}
		logoutRequest, err := http.NewRequest(http.MethodPost, "http://"+address+"/api/v1/auth/logout", bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		logoutRequest.Header.Set("Content-Type", "application/json")
		logoutRequest.Header.Set(security.ClientHeader, clientType)
		if accessToken != "" {
			logoutRequest.Header.Set("Authorization", "Bearer "+accessToken)
		}
		if clientType == "web" {
			logoutRequest.Header.Set("Origin", "http://127.0.0.1:5173")
		}
		response, err := client.Do(logoutRequest)
		if err != nil {
			t.Fatal("正式退出网络请求失败", err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Request-ID") == "" {
			t.Fatal("正式退出响应状态或缓存错误", response.StatusCode, err)
		}
		if status != 200 {
			var rejected dto.ErrorResponse
			if json.Unmarshal(body, &rejected) != nil || rejected.Error.Code != code || rejected.Error.Details != nil || rejected.RequestID != response.Header.Get("X-Request-ID") || len(response.Cookies()) != 0 {
				t.Fatal("正式退出错误结构异常")
			}
			return
		}
		var payload dto.LogoutResponse
		if json.Unmarshal(body, &payload) != nil || !payload.Data.Revoked || payload.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Pragma") != "no-cache" {
			t.Fatal("正式退出成功结构异常")
		}
		cookies := response.Cookies()
		if clientType == "web" {
			if len(cookies) != 1 || cookies[0].Name != security.RefreshCookieName || cookies[0].Value != "" || cookies[0].Path != "/api/v1/auth" || cookies[0].Domain != "" ||
				cookies[0].Secure != expectedSecureCookie || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].MaxAge != -1 || !cookies[0].Expires.Equal(time.Unix(1, 0)) {
				t.Fatal("正式退出的清理 Cookie 属性错误")
			}
		} else if len(cookies) != 0 {
			t.Fatal("正式原生退出修改了 Web Cookie")
		}
		var committed bool
		if err := pool.QueryRowContext(ctx, "SELECT revoked_at IS NOT NULL AND revoked_at=updated_at FROM auth_sessions WHERE user_id=1 AND id=?", sessionID).Scan(&committed); err != nil || !committed {
			t.Fatal("正式退出返回成功时撤销尚未确认入库", err)
		}
	}
	// 正式会话列表按令牌身份核对每条记录归属、设备与当前标记。
	list := func(t *testing.T, query, accessToken string, status int, code, field string) dto.UserSessionListResponse {
		t.Helper()
		listRequest, err := http.NewRequest(http.MethodGet, "http://"+address+"/api/v1/sessions", nil)
		if err != nil {
			t.Fatal(err)
		}
		listRequest.URL.RawQuery = query
		listRequest.Header.Set("Origin", "http://127.0.0.1:5173")
		if accessToken != "" {
			listRequest.Header.Set("Authorization", "Bearer "+accessToken)
		}
		response, err := client.Do(listRequest)
		if err != nil {
			t.Fatal("正式会话列表请求失败", err)
		}
		defer response.Body.Close()
		encoded, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Request-ID") == "" || len(response.Cookies()) != 0 {
			t.Fatal("正式会话列表状态、缓存或读取错误", response.StatusCode, err)
		}
		var payload dto.UserSessionListResponse
		if status != 200 {
			var failure dto.ErrorResponse
			if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("正式列表错误结构异常")
			}
			if field == "" {
				if failure.Error.Details != nil {
					t.Fatal("非字段错误 details 应为空")
				}
			} else if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
				t.Fatal("正式列表查询错误位置不正确")
			}
			return payload
		}
		if json.Unmarshal(encoded, &payload) != nil || payload.Data.Items == nil || payload.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Pragma") != "no-cache" {
			t.Fatal("正式列表成功结构异常")
		}
		identity, err := verifier.Verify(accessToken)
		if err != nil || identity == nil {
			t.Fatal("核对正式列表身份失败", err)
		}
		var previousID uint64
		for _, item := range payload.Data.Items {
			identifier, err := strconv.ParseUint(item.ID, 10, 64)
			if err != nil || (previousID != 0 && identifier >= previousID) || item.Current != (identifier == identity.SessionID) || item.CreatedAt.Location() != time.UTC || item.CreatedAt.Nanosecond()%1000 != 0 {
				t.Fatal("正式列表标识、倒序、当前标记或时间错误", err)
			}
			previousID = identifier
			var deviceID uint64
			var revokedAt, deviceRevokedAt sql.NullTime
			if err := pool.QueryRowContext(ctx, "SELECT sessions.device_id,sessions.revoked_at,devices.revoked_at FROM auth_sessions AS sessions JOIN user_devices AS devices ON devices.user_id=sessions.user_id AND devices.id=sessions.device_id WHERE sessions.user_id=? AND sessions.id=?", identity.UserID, identifier).Scan(&deviceID, &revokedAt, &deviceRevokedAt); err != nil {
				t.Fatal("正式列表包含归属不符的会话", err)
			}
			if item.Device.ID != strconv.FormatUint(deviceID, 10) || (item.RevokedAt != nil) != revokedAt.Valid || (item.DeviceRevokedAt != nil) != deviceRevokedAt.Valid {
				t.Fatal("正式列表设备或可空状态错误")
			}
			if (revokedAt.Valid && !item.RevokedAt.Equal(revokedAt.Time)) || (deviceRevokedAt.Valid && !item.DeviceRevokedAt.Equal(deviceRevokedAt.Time)) {
				t.Fatal("正式列表撤销时间与数据库不一致")
			}
		}
		return payload
	}
	listDevices := func(t *testing.T, query, accessToken string, status int, code, field string) dto.UserDeviceListResponse {
		t.Helper()
		listRequest, err := http.NewRequest(http.MethodGet, "http://"+address+"/api/v1/devices", nil)
		if err != nil {
			t.Fatal(err)
		}
		listRequest.URL.RawQuery = query
		listRequest.Header.Set("Origin", "http://127.0.0.1:5173")
		if accessToken != "" {
			listRequest.Header.Set("Authorization", "Bearer "+accessToken)
		}
		response, err := client.Do(listRequest)
		if err != nil {
			t.Fatal("正式设备列表请求失败", err)
		}
		defer response.Body.Close()
		encoded, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("X-Request-ID") == "" || len(response.Cookies()) != 0 {
			t.Fatal("正式设备列表状态、缓存或读取错误", response.StatusCode, err)
		}
		var payload dto.UserDeviceListResponse
		if status != 200 {
			var failure dto.ErrorResponse
			if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("正式设备列表错误结构异常")
			}
			if field == "" {
				if failure.Error.Details != nil {
					t.Fatal("非字段错误 details 应为空")
				}
			} else if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
				t.Fatal("正式设备列表查询错误位置不正确")
			}
			return payload
		}
		if json.Unmarshal(encoded, &payload) != nil || payload.Data.Items == nil || payload.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Pragma") != "no-cache" {
			t.Fatal("正式设备列表成功结构异常")
		}
		identity, err := verifier.Verify(accessToken)
		if err != nil || identity == nil {
			t.Fatal("核对正式设备列表身份失败", err)
		}
		// 按已验证会话查询设备身份，逐项核对响应与数据库事实。
		var currentDeviceID uint64
		if err := pool.QueryRowContext(ctx, "SELECT device_id FROM auth_sessions WHERE user_id=? AND id=?", identity.UserID, identity.SessionID).Scan(&currentDeviceID); err != nil {
			t.Fatal("核对正式设备列表当前设备失败", err)
		}
		var previousID uint64
		for _, item := range payload.Data.Items {
			identifier, err := strconv.ParseUint(item.ID, 10, 64)
			if err != nil || identifier == 0 || strconv.FormatUint(identifier, 10) != item.ID || (previousID != 0 && identifier >= previousID) ||
				item.Current != (identifier == currentDeviceID) || item.CreatedAt.Location() != time.UTC || item.CreatedAt.Nanosecond()%1000 != 0 {
				t.Fatal("正式设备列表标识、倒序、当前标记或时间错误", err)
			}
			previousID = identifier
			var name, platform string
			var createdAt time.Time
			var lastSeenAt, revokedAt sql.NullTime
			if err := pool.QueryRowContext(ctx, "SELECT name,platform,created_at,last_seen_at,revoked_at FROM user_devices WHERE user_id=? AND id=?", identity.UserID, identifier).Scan(&name, &platform, &createdAt, &lastSeenAt, &revokedAt); err != nil {
				t.Fatal("正式设备列表包含归属不符的设备", err)
			}
			if item.Name != name || item.Platform != platform || !item.CreatedAt.Equal(createdAt) || (item.LastSeenAt != nil) != lastSeenAt.Valid || (item.RevokedAt != nil) != revokedAt.Valid {
				t.Fatal("正式设备列表展示字段或可空状态错误")
			}
			if (lastSeenAt.Valid && (!item.LastSeenAt.Equal(lastSeenAt.Time) || item.LastSeenAt.Location() != time.UTC || item.LastSeenAt.Nanosecond()%1000 != 0)) ||
				(revokedAt.Valid && (!item.RevokedAt.Equal(revokedAt.Time) || item.RevokedAt.Location() != time.UTC || item.RevokedAt.Nanosecond()%1000 != 0)) {
				t.Fatal("正式设备列表活动或撤销时间与数据库不一致")
			}
		}
		return payload
	}
	// 正式改名接口逐项核对提交结果与用户范围内的设备事实。
	type renameSnapshot struct {
		device    domain.UserDevice
		updatedAt time.Time
	}
	readRename := func(t *testing.T, userID uint64, targetID string) renameSnapshot {
		t.Helper()
		var value renameSnapshot
		var lastSeenAt, revokedAt sql.NullTime
		if err := pool.QueryRowContext(ctx, "SELECT id,user_id,name,platform,created_at,last_seen_at,revoked_at,updated_at FROM user_devices WHERE user_id=? AND id=?", userID, targetID).
			Scan(&value.device.ID, &value.device.UserID, &value.device.Name, &value.device.Platform, &value.device.CreatedAt, &lastSeenAt, &revokedAt, &value.updatedAt); err != nil {
			t.Fatal("读取正式改名快照失败", err)
		}
		if lastSeenAt.Valid {
			value.device.LastSeenAt = &lastSeenAt.Time
		}
		if revokedAt.Valid {
			value.device.RevokedAt = &revokedAt.Time
		}
		return value
	}
	renameDevice := func(t *testing.T, targetID, accessToken, name string, status int, code, field string) time.Time {
		t.Helper()
		var before renameSnapshot
		var owner uint64
		if status == 200 {
			identity, err := verifier.Verify(accessToken)
			if err != nil || identity == nil {
				t.Fatal("核对正式改名身份失败", err)
			}
			owner = identity.UserID
			before = readRename(t, owner, targetID)
		}
		body, err := json.Marshal(dto.RenameDeviceRequest{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		message, err := http.NewRequest(http.MethodPatch, "http://"+address+"/api/v1/devices/"+targetID+"?user_id=3&id=23&name=private-query", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		message.Header.Set("Content-Type", "application/json; charset=utf-8")
		message.Header.Set("Origin", "http://127.0.0.1:5173")
		if accessToken != "" {
			message.Header.Set("Authorization", "Bearer "+accessToken)
		}
		response, err := client.Do(message)
		if err != nil {
			t.Fatal("正式改名请求失败", err)
		}
		defer response.Body.Close()
		encoded, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status || response.Header.Get("X-Request-ID") == "" || response.Header.Get("Cache-Control") != "no-store" || (status != 401 && status != 403 && response.Header.Get("Pragma") != "no-cache") || len(response.Cookies()) != 0 {
			t.Fatal("正式改名状态、缓存或读取错误", response.StatusCode, err)
		}
		if status != 200 {
			var failure dto.ErrorResponse
			if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("正式改名错误响应异常")
			}
			if field == "" {
				if failure.Error.Details != nil {
					t.Fatal("非字段错误 details 应为空")
				}
			} else if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
				t.Fatal("正式改名字段位置错误")
			}
			if status == 404 && (failure.Error.Message != "设备不存在" || response.Header.Get("WWW-Authenticate") != "") {
				t.Fatal("正式设备目标与认证错误混淆")
			}
			if status == 401 && response.Header.Get("WWW-Authenticate") == "" {
				t.Fatal("改名认证错误缺少 Bearer 提示")
			}
			return time.Time{}
		}
		var payload dto.RenameDeviceResponse
		if json.Unmarshal(encoded, &payload) != nil || payload.RequestID != response.Header.Get("X-Request-ID") || payload.Data.Device.ID != targetID || payload.Data.Device.Name != name {
			t.Fatal("正式改名成功 JSON 异常")
		}
		after := readRename(t, owner, targetID)
		expected := before
		expected.device.Name, expected.updatedAt = name, after.updatedAt
		if !reflect.DeepEqual(after, expected) || after.updatedAt.Nanosecond()%1000 != 0 || (before.device.Name == name && !after.updatedAt.Equal(before.updatedAt)) {
			t.Fatal("正式改名字段或同名重试的更新时间错误")
		}
		data, err := dto.NewRenameDeviceData(&after.device)
		if err != nil || !reflect.DeepEqual(payload.Data, *data) {
			t.Fatal("正式改名响应与提交事实不一致", err)
		}
		return after.updatedAt
	}
	// 按用户与设备读取全部关联会话，为两表提交和重试提供快照。
	type deviceSessionSnapshot struct {
		revokedAt sql.NullTime
		updatedAt time.Time
	}
	readDeviceSessions := func(t *testing.T, userID uint64, deviceID string) map[uint64]deviceSessionSnapshot {
		t.Helper()
		rows, err := pool.QueryContext(ctx, "SELECT id,revoked_at,updated_at FROM auth_sessions WHERE user_id=? AND device_id=?", userID, deviceID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		values := make(map[uint64]deviceSessionSnapshot)
		for rows.Next() {
			var sessionID uint64
			var value deviceSessionSnapshot
			if err := rows.Scan(&sessionID, &value.revokedAt, &value.updatedAt); err != nil {
				t.Fatal(err)
			}
			values[sessionID] = value
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return values
	}
	revokeDevice := func(t *testing.T, targetID, accessToken string, status int, code, field string) time.Time {
		t.Helper()
		// 1. 保存成功目标的设备及全部会话快照。
		var owner uint64
		var before renameSnapshot
		var sessionsBefore map[uint64]deviceSessionSnapshot
		if status == 200 {
			identity, err := verifier.Verify(accessToken)
			if err != nil || identity == nil {
				t.Fatal("核对设备撤销身份失败", err)
			}
			owner = identity.UserID
			before = readRename(t, owner, targetID)
			sessionsBefore = readDeviceSessions(t, owner, targetID)
		}
		// 2. 请求正式路径，核对响应及错误分类。
		revokeRequest, err := http.NewRequest(http.MethodDelete, "http://"+address+"/api/v1/devices/"+targetID+"?user_id=3&id=23&note=private-query", nil)
		if err != nil {
			t.Fatal(err)
		}
		revokeRequest.Header.Set("Origin", "http://127.0.0.1:5173")
		if accessToken != "" {
			revokeRequest.Header.Set("Authorization", "Bearer "+accessToken)
		}
		response, err := client.Do(revokeRequest)
		if err != nil {
			t.Fatal("正式设备撤销请求失败", err)
		}
		defer response.Body.Close()
		encoded, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" ||
			response.Header.Get("X-Request-ID") == "" || len(response.Cookies()) != 0 ||
			response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Content-Encoding") != "" ||
			response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" {
			t.Fatal("正式撤销状态、缓存或读取错误", response.StatusCode, err)
		}
		if status != 200 {
			var rejected dto.ErrorResponse
			if json.Unmarshal(encoded, &rejected) != nil || rejected.Error.Code != code || rejected.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("正式撤销错误结构或请求 ID 异常")
			}
			if field == "" {
				if rejected.Error.Details != nil {
					t.Fatal("非字段错误 details 应为空")
				}
			} else if len(rejected.Error.Details) != 1 || rejected.Error.Details["field"] != field {
				t.Fatal("正式撤销路径错误位置不正确")
			}
			if status == 404 && (rejected.Error.Message != "设备不存在" || response.Header.Get("WWW-Authenticate") != "") {
				t.Fatal("正式目标错误与认证错误混淆")
			}
			if status == 401 && response.Header.Get("WWW-Authenticate") == "" {
				t.Fatal("正式撤销认证错误缺少 Bearer 提示")
			}
			return time.Time{}
		}
		var payload dto.RevokeDeviceResponse
		if json.Unmarshal(encoded, &payload) != nil || !payload.Data.Revoked || payload.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Pragma") != "no-cache" {
			t.Fatal("正式撤销成功结构错误")
		}
		// 3. 成功响应后核对设备与全部会话的提交事实及历史时间。
		after := readRename(t, owner, targetID)
		if after.device.RevokedAt == nil || after.device.RevokedAt.Nanosecond()%1000 != 0 {
			t.Fatal("设备撤销返回成功时缺少已提交时间")
		}
		expected := before
		if before.device.RevokedAt == nil {
			expected.device.RevokedAt, expected.updatedAt = after.device.RevokedAt, *after.device.RevokedAt
		}
		if !reflect.DeepEqual(after, expected) {
			t.Fatal("设备撤销改变了其他字段或历史撤销时间")
		}
		sessionsAfter := readDeviceSessions(t, owner, targetID)
		if len(sessionsAfter) != len(sessionsBefore) {
			t.Fatal("设备撤销改变了关联会话数量")
		}
		for sessionID, previous := range sessionsBefore {
			current, exists := sessionsAfter[sessionID]
			if !exists || !current.revokedAt.Valid || current.revokedAt.Time.Nanosecond()%1000 != 0 {
				t.Fatal("设备撤销成功后仍存在未撤销会话")
			}
			if previous.revokedAt.Valid {
				if current != previous {
					t.Fatal("设备撤销覆盖了会话历史时间")
				}
			} else if !current.revokedAt.Time.Equal(current.updatedAt) {
				t.Fatal("关联会话撤销与更新时间不一致")
			}
		}
		return *after.device.RevokedAt
	}
	// 正式指定会话撤销接口按调用者归属核对目标提交时间。
	revoke := func(t *testing.T, targetID, accessToken string, status int, code, field string) time.Time {
		t.Helper()
		revokeRequest, err := http.NewRequest(http.MethodDelete, "http://"+address+"/api/v1/sessions/"+targetID+"?user_id=3&id=27&note=private-query", nil)
		if err != nil {
			t.Fatal(err)
		}
		revokeRequest.Header.Set("Origin", "http://127.0.0.1:5173")
		if accessToken != "" {
			revokeRequest.Header.Set("Authorization", "Bearer "+accessToken)
		}
		response, err := client.Do(revokeRequest)
		if err != nil {
			t.Fatal("正式会话撤销请求失败", err)
		}
		defer response.Body.Close()
		encoded, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != status || response.Header.Get("Cache-Control") != "no-store" ||
			response.Header.Get("X-Request-ID") == "" || len(response.Cookies()) != 0 {
			t.Fatal("正式撤销状态、缓存或读取错误", response.StatusCode, err)
		}
		if status != 200 {
			var rejected dto.ErrorResponse
			if json.Unmarshal(encoded, &rejected) != nil || rejected.Error.Code != code || rejected.RequestID != response.Header.Get("X-Request-ID") {
				t.Fatal("正式撤销错误结构或请求 ID 异常")
			}
			if field == "" {
				if rejected.Error.Details != nil {
					t.Fatal("非字段错误 details 应为空")
				}
			} else if len(rejected.Error.Details) != 1 || rejected.Error.Details["field"] != field {
				t.Fatal("正式撤销路径错误位置不正确")
			}
			if status == 404 && (rejected.Error.Message != "会话不存在" || response.Header.Get("WWW-Authenticate") != "") {
				t.Fatal("正式目标错误与认证错误混淆")
			}
			if status == 401 && response.Header.Get("WWW-Authenticate") == "" {
				t.Fatal("正式撤销认证错误缺少 Bearer 提示")
			}
			return time.Time{}
		}
		var payload dto.RevokeSessionResponse
		if json.Unmarshal(encoded, &payload) != nil || !payload.Data.Revoked || payload.RequestID != response.Header.Get("X-Request-ID") || response.Header.Get("Pragma") != "no-cache" {
			t.Fatal("正式撤销成功结构错误")
		}
		identity, err := verifier.Verify(accessToken)
		if err != nil || identity == nil {
			t.Fatal("核对正式撤销身份失败", err)
		}
		var revokedAt, updatedAt time.Time
		if err := pool.QueryRowContext(ctx, "SELECT revoked_at,updated_at FROM auth_sessions WHERE user_id=? AND id=?", identity.UserID, targetID).Scan(&revokedAt, &updatedAt); err != nil ||
			revokedAt.IsZero() || !revokedAt.Equal(updatedAt) || revokedAt.Nanosecond()%1000 != 0 {
			t.Fatal("正式撤销返回成功时目标状态未正确提交", err)
		}
		return revokedAt
	}
	revokeDevice(t, "0", "", 401, "AUTH_REQUIRED", "")
	revokeDevice(t, "0", token.Token, 400, "INVALID_ARGUMENT", "id")
	revokeDevice(t, "18446744073709551616", token.Token, 400, "INVALID_ARGUMENT", "id")
	revokeDevice(t, "18446744073709551615", token.Token, 404, "DEVICE_NOT_FOUND", "")
	revokeDevice(t, "23", token.Token, 404, "DEVICE_NOT_FOUND", "")
	revokeDevice(t, "13", foreignToken.Token, 404, "DEVICE_NOT_FOUND", "")
	revoke(t, "0", "", 401, "AUTH_REQUIRED", "")
	revoke(t, "0", token.Token, 400, "INVALID_ARGUMENT", "id")
	revoke(t, "18446744073709551616", token.Token, 400, "INVALID_ARGUMENT", "id")
	revoke(t, "18446744073709551615", token.Token, 404, "SESSION_NOT_FOUND", "")
	revoke(t, "27", token.Token, 404, "SESSION_NOT_FOUND", "")
	revoke(t, "17", foreignToken.Token, 404, "SESSION_NOT_FOUND", "")
	list(t, "limit=0", "", 401, "AUTH_REQUIRED", "")
	list(t, "limit=0", token.Token, 400, "INVALID_ARGUMENT", "limit")
	list(t, "before_id=%ZZ", token.Token, 400, "INVALID_ARGUMENT", "query")
	listDevices(t, "limit=0", "", 401, "AUTH_REQUIRED", "")
	listDevices(t, "limit=0", token.Token, 400, "INVALID_ARGUMENT", "limit")
	listDevices(t, "before_id=%ZZ", token.Token, 400, "INVALID_ARGUMENT", "query")
	listDevices(t, "before_id=18446744073709551616", token.Token, 400, "INVALID_ARGUMENT", "before_id")
	listDevices(t, "limit=1&limit=2", token.Token, 400, "INVALID_ARGUMENT", "limit")
	renameDevice(t, "0", "", "新名", 401, "AUTH_REQUIRED", "")
	renameDevice(t, "0", token.Token, "新名", 400, "INVALID_ARGUMENT", "id")
	renameDevice(t, "13", token.Token, "", 400, "INVALID_ARGUMENT", "name")
	renameDevice(t, "18446744073709551616", token.Token, "新名", 400, "INVALID_ARGUMENT", "id")
	renameDevice(t, "18446744073709551615", token.Token, "新名", 404, "DEVICE_NOT_FOUND", "")
	renameDevice(t, "23", token.Token, "新名", 404, "DEVICE_NOT_FOUND", "")
	renameDevice(t, "13", foreignToken.Token, "新名", 404, "DEVICE_NOT_FOUND", "")
	const webInstallation = "28ee758a-0727-42de-ae7d-3bb33155e148"
	const nativeInstallation = "edbf7e35-36ea-45a5-bdb9-58e76b46fd5e"
	logout(t, "web", "", "", 401, "AUTH_REQUIRED")
	login(t, "web", webInstallation, "wrong-password", 401)
	webLogin, webCookies := login(t, "web", webInstallation, "fixture-password", 200)
	if webLogin.Data.Device.ID != "13" || webLogin.Data.Device.Name != "启动测试设备" {
		t.Fatal("正式登录没有复用现有设备")
	}
	nativeLogin, _ := login(t, "native", nativeInstallation, "fixture-password", 200)
	if nativeLogin.Data.Device.ID == "13" {
		t.Fatal("新安装复用了其他设备")
	}
	repeatedLogin, repeatedCookies := login(t, "web", webInstallation, "fixture-password", 200)
	if repeatedLogin.Data.Device.ID != "13" || repeatedLogin.Data.Session.ID == webLogin.Data.Session.ID {
		t.Fatal("重新登录的设备复用或独立会话错误")
	}
	webRefreshed, webRaw := refresh(t, "web", webCookies[0].Value, webLogin.Data.Session.ID, 200, 2)
	refresh(t, "web", webCookies[0].Value, webLogin.Data.Session.ID, 401, 0)
	nativeRefreshed, nativeRaw := refresh(t, "native", nativeLogin.Data.RefreshToken, nativeLogin.Data.Session.ID, 200, 2)
	refresh(t, "native", nativeLogin.Data.RefreshToken, nativeLogin.Data.Session.ID, 401, 0)
	// 登录及刷新所得访问令牌通过正式列表路径完成分页和归属验证。
	firstPage := list(t, "limit=2&user_id=3&note=private-query", webRefreshed.Data.AccessToken, 200, "", "")
	if len(firstPage.Data.Items) != 2 || !firstPage.Data.HasMore || firstPage.Data.Items[0].ID != repeatedLogin.Data.Session.ID {
		t.Fatal("正式列表首页的分页或倒序错误")
	}
	secondPage := list(t, "before_id="+firstPage.Data.Items[1].ID+"&limit=2", webRefreshed.Data.AccessToken, 200, "", "")
	if len(secondPage.Data.Items) != 2 || secondPage.Data.HasMore || secondPage.Data.Items[0].ID != webLogin.Data.Session.ID || !secondPage.Data.Items[0].Current {
		t.Fatal("正式列表后续页当前标记错误")
	}
	emptyPage := list(t, "before_id=17", webRefreshed.Data.AccessToken, 200, "", "")
	if len(emptyPage.Data.Items) != 0 || emptyPage.Data.HasMore {
		t.Fatal("正式列表末页边界错误")
	}
	maximumPage := list(t, "before_id=18446744073709551615&limit=100", nativeRefreshed.Data.AccessToken, 200, "", "")
	if len(maximumPage.Data.Items) != 4 || maximumPage.Data.HasMore {
		t.Fatal("正式列表最大边界或账号隔离错误")
	}
	foreignPage := list(t, "user_id=1", foreignToken.Token, 200, "", "")
	if len(foreignPage.Data.Items) != 1 || foreignPage.Data.Items[0].ID != "27" || !foreignPage.Data.Items[0].Current {
		t.Fatal("正式列表的其他账号身份错误")
	}
	// 同设备登录复用设备项，按设备 ID 分页并从当前会话定位 Current。
	deviceFirst := listDevices(t, "limit=1&user_id=3&device_id=23&note=private-query", webRefreshed.Data.AccessToken, 200, "", "")
	if len(deviceFirst.Data.Items) != 1 || !deviceFirst.Data.HasMore || deviceFirst.Data.Items[0].ID != nativeLogin.Data.Device.ID || deviceFirst.Data.Items[0].Current {
		t.Fatal("正式设备列表首页或同设备去重错误")
	}
	deviceLast := listDevices(t, "before_id="+deviceFirst.Data.Items[0].ID+"&limit=1", webRefreshed.Data.AccessToken, 200, "", "")
	if len(deviceLast.Data.Items) != 1 || deviceLast.Data.HasMore || deviceLast.Data.Items[0].ID != "13" || !deviceLast.Data.Items[0].Current {
		t.Fatal("正式设备列表后续页的当前设备错误")
	}
	deviceEmpty := listDevices(t, "before_id=13", webRefreshed.Data.AccessToken, 200, "", "")
	if len(deviceEmpty.Data.Items) != 0 || deviceEmpty.Data.HasMore {
		t.Fatal("正式设备列表空页错误")
	}
	deviceMaximum := listDevices(t, "before_id=18446744073709551615&limit=100", nativeRefreshed.Data.AccessToken, 200, "", "")
	if len(deviceMaximum.Data.Items) != 2 || deviceMaximum.Data.HasMore || !deviceMaximum.Data.Items[0].Current {
		t.Fatal("正式设备列表最大边界或当前设备错误")
	}
	deviceRepeated := listDevices(t, "", repeatedLogin.Data.AccessToken, 200, "", "")
	if len(deviceRepeated.Data.Items) != 2 || !deviceRepeated.Data.Items[1].Current {
		t.Fatal("同设备其他会话未使用相同设备项与当前标记")
	}
	deviceForeign := listDevices(t, "user_id=1", foreignToken.Token, 200, "", "")
	if len(deviceForeign.Data.Items) != 1 || deviceForeign.Data.Items[0].ID != "23" || !deviceForeign.Data.Items[0].Current {
		t.Fatal("正式设备列表未按认证账号隔离")
	}
	// 第一轮启动中分别退出独立会话，原有会话继续用于刷新及重启验证。
	logout(t, "web", repeatedLogin.Data.AccessToken, repeatedLogin.Data.Session.ID, 200, "")
	request(t, "/api/v1/me", repeatedLogin.Data.AccessToken, 401)
	refresh(t, "web", repeatedCookies[0].Value, repeatedLogin.Data.Session.ID, 401, 0)
	request(t, "/api/v1/me", webRefreshed.Data.AccessToken, 200)
	nativeExited, _ := login(t, "native", nativeInstallation, "fixture-password", 200)
	if nativeExited.Data.Device.ID != nativeLogin.Data.Device.ID {
		t.Fatal("正式退出测试未建立同设备独立会话")
	}
	logout(t, "native", nativeExited.Data.AccessToken, nativeExited.Data.Session.ID, 200, "")
	request(t, "/api/v1/me", nativeExited.Data.AccessToken, 401)
	refresh(t, "native", nativeExited.Data.RefreshToken, nativeExited.Data.Session.ID, 401, 0)
	request(t, "/api/v1/me", nativeRefreshed.Data.AccessToken, 200)
	expiredAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET created_at=?,last_seen_at=?,expires_at=? WHERE id=?", expiredAt.Add(-time.Hour), expiredAt, expiredAt, nativeExited.Data.Session.ID); err != nil {
		t.Fatal(err)
	}
	history := list(t, "", webRefreshed.Data.AccessToken, 200, "", "")
	if len(history.Data.Items) != 5 || history.Data.Items[0].ID != nativeExited.Data.Session.ID || history.Data.Items[0].RevokedAt == nil || history.Data.Items[0].LastSeenAt == nil || !history.Data.Items[0].ExpiresAt.Equal(expiredAt) {
		t.Fatal("正式列表未交付退出后的历史时间事实")
	}
	// 通过正式登录创建两个独立目标，分别撤销其他设备与同设备会话。
	remoteTarget, _ := login(t, "native", nativeInstallation, "fixture-password", 200)
	remoteRevokedAt := revoke(t, remoteTarget.Data.Session.ID, webRefreshed.Data.AccessToken, 200, "", "")
	if repeatedAt := revoke(t, remoteTarget.Data.Session.ID, webRefreshed.Data.AccessToken, 200, "", ""); !repeatedAt.Equal(remoteRevokedAt) {
		t.Fatal("正式重复撤销覆盖了首次撤销时间")
	}
	request(t, "/api/v1/me", remoteTarget.Data.AccessToken, 401)
	refresh(t, "native", remoteTarget.Data.RefreshToken, remoteTarget.Data.Session.ID, 401, 0)
	request(t, "/api/v1/me", nativeRefreshed.Data.AccessToken, 200)
	sameDeviceTarget, sameDeviceCookies := login(t, "web", webInstallation, "fixture-password", 200)
	if sameDeviceTarget.Data.Device.ID != webLogin.Data.Device.ID {
		t.Fatal("正式撤销没有建立同设备目标")
	}
	revoke(t, sameDeviceTarget.Data.Session.ID, webRefreshed.Data.AccessToken, 200, "", "")
	request(t, "/api/v1/me", sameDeviceTarget.Data.AccessToken, 401)
	refresh(t, "web", sameDeviceCookies[0].Value, sameDeviceTarget.Data.Session.ID, 401, 0)
	request(t, "/api/v1/me", webRefreshed.Data.AccessToken, 200)
	revokeHistory := list(t, "", webRefreshed.Data.AccessToken, 200, "", "")
	if len(revokeHistory.Data.Items) != 7 || revokeHistory.Data.Items[0].ID != sameDeviceTarget.Data.Session.ID || revokeHistory.Data.Items[0].RevokedAt == nil {
		t.Fatal("正式列表未呈现指定撤销后的历史状态")
	}

	webDeviceName, nativeDeviceName := "  重启后仍保留的浏览器 🎮  ", "改名后的移动设备"
	webRenamedAt := renameDevice(t, "13", webRefreshed.Data.AccessToken, webDeviceName, 200, "", "")
	if repeatedAt := renameDevice(t, "13", webRefreshed.Data.AccessToken, webDeviceName, 200, "", ""); !repeatedAt.Equal(webRenamedAt) {
		t.Fatal("正式同名重试改变更新时间")
	}
	renameDevice(t, nativeLogin.Data.Device.ID, webRefreshed.Data.AccessToken, nativeDeviceName, 200, "", "")
	// 使用正式登录建立同设备双会话，撤销后跨重启检查两表事实。
	const remoteInstallation = "ca67141f-9801-40be-aa25-d7cb9cdc74e6"
	deviceTarget, _ := login(t, "native", remoteInstallation, "fixture-password", 200)
	deviceSibling, _ := login(t, "native", remoteInstallation, "fixture-password", 200)
	if deviceTarget.Data.Device.ID != deviceSibling.Data.Device.ID {
		t.Fatal("设备撤销测试没有建立同设备双会话")
	}
	deviceRevokedAt := revokeDevice(t, deviceTarget.Data.Device.ID, webRefreshed.Data.AccessToken, 200, "", "")
	if repeatedAt := revokeDevice(t, deviceTarget.Data.Device.ID, webRefreshed.Data.AccessToken, 200, "", ""); !repeatedAt.Equal(deviceRevokedAt) {
		t.Fatal("正式设备重复撤销覆盖首次时间")
	}
	for _, current := range []dto.LoginResponse{deviceTarget, deviceSibling} {
		request(t, "/api/v1/me", current.Data.AccessToken, 401)
		refresh(t, "native", current.Data.RefreshToken, current.Data.Session.ID, 401, 0)
		revokeDevice(t, "13", current.Data.AccessToken, 401, "SESSION_REVOKED", "")
	}
	deviceSessionsBeforeRestart := readDeviceSessions(t, 1, deviceTarget.Data.Device.ID)
	devicesBeforeRestart := listDevices(t, "", webRefreshed.Data.AccessToken, 200, "", "")
	directoryBeforeRestart := listDirectory(t, "parent_id=12&limit=2", webRefreshed.Data.AccessToken, 200, "", "")
	detailBeforeRestart := getEntryDetail(t, "23", webRefreshed.Data.AccessToken, 200, "", "")
	if !reflect.DeepEqual(detailFirst.Data, detailBeforeRestart.Data) {
		t.Fatal("设备改名后来源名称快照或详情事实出现变化")
	}
	if !reflect.DeepEqual(directoryFirst.Data, directoryBeforeRestart.Data) {
		t.Fatal("同用户不同会话读取相同目录出现不一致")
	}
	directoryFromOtherDevice := listDirectory(t, directoryNextQuery, nativeRefreshed.Data.AccessToken, 200, "", "")
	if !reflect.DeepEqual(directoryLast.Data, directoryFromOtherDevice.Data) {
		t.Fatal("同用户的其他设备未能使用目录游标")
	}

	// 正式创建与幂等保存经过真实启动装配，创建快照跨服务重启复用。
	createdDirectoryBeforeRestart := verifyServerDirectoryCreation(t, ctx, pool, "http://"+address, webRefreshed.Data.AccessToken, foreignToken.Token)
	createdDetailBeforeRestart := getEntryDetail(t, createdDirectoryBeforeRestart.Data.Entry.ID, webRefreshed.Data.AccessToken, 200, "", "")
	createdDirectoryPage := listDirectory(t, "parent_id=15", webRefreshed.Data.AccessToken, 200, "", "")
	if createdDetailBeforeRestart.Data.Name != "正式创建后改名" || len(createdDirectoryPage.Data.Items) != 1 || createdDirectoryPage.Data.Items[0].ID != createdDirectoryBeforeRestart.Data.Entry.ID {
		t.Fatal("正式创建的目录未衔接列表与详情读取")
	}
	// 正式改名覆盖文件与目录，重启后继续复用首次快照。
	renamedEntryBeforeRestart := verifyServerEntryRename(t, ctx, pool, "http://"+address, webRefreshed.Data.AccessToken, foreignToken.Token)
	renamedDetailBeforeRestart := getEntryDetail(t, "42", webRefreshed.Data.AccessToken, 200, "", "")
	if renamedDetailBeforeRestart.Data.Name != "正式后续.ZIP" || renamedDetailBeforeRestart.Data.Revision != "3" || renamedDetailBeforeRestart.Data.Kind != "archive" {
		t.Fatal("正式改名未衔接当前详情")
	}
	renamedPage := listDirectory(t, "parent_id=11", webRefreshed.Data.AccessToken, 200, "", "")
	foundRenamed := false
	for _, entry := range renamedPage.Data.Items {
		if entry.ID == "42" && entry.Name == "正式后续.ZIP" && entry.Revision == "3" {
			foundRenamed = true
		}
	}
	if !foundRenamed {
		t.Fatal("正式改名未衔接当前列表")
	}
	// 正式移动衔接详情、双方列表及后代父链，原始快照跨重启复用。
	movedEntryBeforeRestart := verifyServerEntryMove(t, ctx, pool, "http://"+address, webRefreshed.Data.AccessToken, foreignToken.Token)
	movedDetailBeforeRestart := getEntryDetail(t, "70001", webRefreshed.Data.AccessToken, 200, "", "")
	if movedDetailBeforeRestart.Data.ParentID == nil || *movedDetailBeforeRestart.Data.ParentID != "70004" || movedDetailBeforeRestart.Data.Revision != "3" {
		t.Fatal("正式移动未衔接当前详情")
	}
	movedPageBeforeRestart := listDirectory(t, "parent_id=70004", webRefreshed.Data.AccessToken, 200, "", "")
	if len(movedPageBeforeRestart.Data.Items) != 1 || movedPageBeforeRestart.Data.Items[0].ID != "70001" {
		t.Fatal("正式移动未衔接目的地列表")
	}
	movedSourcePage := listDirectory(t, "parent_id=70002", webRefreshed.Data.AccessToken, 200, "", "")
	if len(movedSourcePage.Data.Items) != 0 {
		t.Fatal("原父目录仍列出已移动项")
	}
	movedChildPage := listDirectory(t, "parent_id=70005", webRefreshed.Data.AccessToken, 200, "", "")
	if len(movedChildPage.Data.Items) != 1 || movedChildPage.Data.Items[0].ID != "70006" || len(movedChildPage.Data.Breadcrumbs) != 3 || movedChildPage.Data.Breadcrumbs[1].ID != "70003" || movedChildPage.Data.Breadcrumbs[2].ID != "70005" {
		t.Fatal("正式目录移动后的后代父链错误")
	}

	// 正式收藏衔接详情，收藏时间及最近访问事实跨服务重启复验。
	starredBeforeRestart := verifyServerSetStarred(t, ctx, pool, "http://"+address, webRefreshed.Data.AccessToken, foreignToken.Token)
	starredDetailBeforeRestart := getEntryDetail(t, "80001", webRefreshed.Data.AccessToken, 200, "", "")
	if !starredDetailBeforeRestart.Data.Starred || starredDetailBeforeRestart.Data.StarredAt == nil || !starredDetailBeforeRestart.Data.StarredAt.Equal(*starredBeforeRestart.Data.StarredAt) || starredDetailBeforeRestart.Data.Revision != "7" {
		t.Fatal("正式收藏未衔接详情事实")
	}
	starredPageBeforeRestart := listDirectory(t, "parent_id=80002", webRefreshed.Data.AccessToken, 200, "", "")
	if len(starredPageBeforeRestart.Data.Items) != 2 {
		t.Fatal("正式收藏目录列表错误")
	}

	// 正式访问衔接详情，数据库访问时间及原收藏跨服务重启复验。
	accessBeforeRestart := verifyServerRecordAccess(t, ctx, pool, "http://"+address, webRefreshed.Data.AccessToken, foreignToken.Token)
	accessDetailBeforeRestart := getEntryDetail(t, "90001", webRefreshed.Data.AccessToken, 200, "", "")
	if accessDetailBeforeRestart.Data.LastOpenedAt == nil || !accessDetailBeforeRestart.Data.LastOpenedAt.Equal(accessBeforeRestart.Data.LastOpenedAt) || !accessDetailBeforeRestart.Data.Starred || accessDetailBeforeRestart.Data.Revision != "9" {
		t.Fatal("正式访问未衔接详情事实")
	}

	// 正式全局查询连接跨目录分页和预算空页，保存原游标供重启续用。
	globalBeforeRestart, globalBudgetBeforeRestart := verifyServerGlobalEntries(t, ctx, pool, "http://"+address, webRefreshed.Data.AccessToken, foreignToken.Token, jwtConfig.SigningKey)
	loginSecrets = append(loginSecrets, *globalBeforeRestart.Data.NextCursor, *globalBudgetBeforeRestart.Data.NextCursor)
	// 正式收藏查询复用现有元数据夹具，通过写入及列表完成闭环。
	starredListBeforeRestart, starredBudgetBeforeRestart := verifyServerStarredEntries(t, ctx, pool, "http://"+address, webRefreshed.Data.AccessToken, foreignToken.Token, *globalBeforeRestart.Data.NextCursor, jwtConfig.SigningKey)
	loginSecrets = append(loginSecrets, *starredListBeforeRestart.Data.NextCursor, *starredBudgetBeforeRestart.Data.NextCursor)
	recentBeforeRestart, recentBudgetBeforeRestart := verifyServerRecentEntries(t, ctx, pool, "http://"+address, webRefreshed.Data.AccessToken, foreignToken.Token, *globalBeforeRestart.Data.NextCursor, *starredListBeforeRestart.Data.NextCursor, jwtConfig.SigningKey)
	loginSecrets = append(loginSecrets, *recentBeforeRestart.Data.NextCursor, *recentBudgetBeforeRestart.Data.NextCursor)
	// 正式名称查询组合 all/starred，持久化签名位置用于重启续读。
	nameBeforeRestart, nameStarredBeforeRestart, nameBudgetBeforeRestart, nameLongBeforeRestart := verifyServerNameEntries(t, ctx, pool, "http://"+address, webRefreshed.Data.AccessToken, foreignToken.Token, *globalBeforeRestart.Data.NextCursor, *recentBeforeRestart.Data.NextCursor, jwtConfig.SigningKey)
	loginSecrets = append(loginSecrets, *nameBeforeRestart.Data.NextCursor, *nameStarredBeforeRestart.Data.NextCursor, *nameBudgetBeforeRestart.Data.NextCursor, *nameLongBeforeRestart.Data.NextCursor)
	// 正式大小查询组合五类分派，保存普通、收藏、预算及同大小位置。
	sizeBeforeRestart, sizeStarredBeforeRestart, sizeBudgetBeforeRestart, sizeTieBeforeRestart := verifyServerSizeEntries(t, ctx, pool, "http://"+address, webRefreshed.Data.AccessToken, foreignToken.Token, *globalBeforeRestart.Data.NextCursor, *recentBeforeRestart.Data.NextCursor, *nameBeforeRestart.Data.NextCursor, jwtConfig.SigningKey)
	loginSecrets = append(loginSecrets, *sizeBeforeRestart.Data.NextCursor, *sizeStarredBeforeRestart.Data.NextCursor, *sizeBudgetBeforeRestart.Data.NextCursor, *sizeTieBeforeRestart.Data.NextCursor)

	// 正式任务查询贯穿已有授权与持久记录，保存结果供重启后对照。
	uploadBeforeRestart, completedUploadBeforeRestart := verifyServerUploadTasks(t, ctx, pool, "http://"+address, webRefreshed.Data.AccessToken, nativeRefreshed.Data.AccessToken, foreignToken.Token)
	// 上传创建使用既有账号 3，独立对照配额、任务与稳定结果。
	createdUploadBeforeRestart, uploadCreationBeforeRestart := verifyServerUploadCreation(t, ctx, pool, "http://"+address, foreignToken.Token, webRefreshed.Data.AccessToken)

	// 3. 重启后沿用原 JWT，再验证数据库禁用及会话撤销立即影响后续请求。
	secretPath := filepath.Join(configurationDirectory, "CloudNest", "jwt-secret")
	before, err := os.Stat(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	stopFirst()
	t.Setenv("CLOUDNEST_COOKIE_SECURE", "true")
	expectedSecureCookie = true
	stopSecond := start()
	verifyServerUploadCreationAfterRestart(t, ctx, pool, "http://"+address, foreignToken.Token, createdUploadBeforeRestart, uploadCreationBeforeRestart)
	if uploadStorage != nil {
		t.Run("upload content", func(t *testing.T) {
			verifyServerUploadContent(t, ctx, pool, uploadStorage, "http://"+address, foreignToken.Token, webRefreshed.Data.AccessToken)
		})
	}
	// 两类任务数据跨真实服务重启保持一致，各次请求使用当前请求 ID。
	for _, fixture := range []dto.UploadTaskResponse{uploadBeforeRestart, completedUploadBeforeRestart} {
		after := requestServerUploadTask(t, "http://"+address, webRefreshed.Data.AccessToken, fixture.Data.ID, 200, "", "")
		if !reflect.DeepEqual(after.Data, fixture.Data) || after.RequestID == fixture.RequestID {
			t.Fatal("正式上传任务的重启数据或请求 ID 错误")
		}
	}
	// 名称普通、收藏、预算和长键首页分别复用原文本，并与首次事实对照。
	for _, fixture := range []struct {
		raw     string
		before  dto.GlobalEntryPageResponse
		nextIDs []string
	}{
		{"scope=all&sort=name_asc&kind=document&q=" + url.QueryEscape("全局%_") + "&limit=1", nameBeforeRestart, []string{"100003"}},
		{"scope=starred&sort=name_asc&kind=document&q=" + url.QueryEscape("全局%_") + "&limit=1", nameStarredBeforeRestart, []string{"100003"}},
		{"scope=all&sort=name_asc&q=name-budget-&kind=document&limit=2", nameBudgetBeforeRestart, []string{"300001", "300003"}},
		{"scope=all&sort=name_asc&q=name-long-&kind=document&limit=1", nameLongBeforeRestart, []string{"300503"}},
	} {
		page := requestServerGlobalEntries(t, "http://"+address, webRefreshed.Data.AccessToken, fixture.raw, 200, "", "")
		if !reflect.DeepEqual(page.Data, fixture.before.Data) || page.RequestID == fixture.before.RequestID {
			t.Fatal("名称首页、签名或请求 ID 未按预期跨重启保留")
		}
		nextQuery, err := url.ParseQuery(fixture.raw)
		if err != nil {
			t.Fatal("名称重启续读参数错误", err)
		}
		nextQuery.Set("cursor", *fixture.before.Data.NextCursor)
		nextQuery.Set("limit", "100")
		next := requestServerGlobalEntries(t, "http://"+address, webRefreshed.Data.AccessToken, nextQuery.Encode(), 200, "", "")
		if len(next.Data.Items) != len(fixture.nextIDs) || next.Data.NextCursor != nil {
			t.Fatal("名称原游标未跨重启正确结束", len(next.Data.Items), next.Data.NextCursor != nil)
		}
		for index, entryID := range fixture.nextIDs {
			if next.Data.Items[index].ID != entryID {
				t.Fatal("名称原游标跨重启读取条目错误")
			}
		}
	}

	// 大小普通、收藏、预算和同大小首页分别复用原文本，并与首次事实对照。
	for _, fixture := range []struct {
		raw     string
		before  dto.GlobalEntryPageResponse
		nextIDs []string
	}{
		{"scope=all&sort=size_desc&kind=document&q=" + url.QueryEscape("全局%_") + "&limit=1", sizeBeforeRestart, []string{"100001"}},
		{"scope=starred&sort=size_desc&kind=document&q=" + url.QueryEscape("全局%_") + "&limit=1", sizeStarredBeforeRestart, []string{"100001"}},
		{"scope=all&sort=size_desc&q=size-budget-&kind=document&limit=2", sizeBudgetBeforeRestart, []string{"400001", "400003"}},
		{"scope=all&sort=size_desc&q=size-tie-&kind=document&limit=1", sizeTieBeforeRestart, []string{"400501"}},
	} {
		page := requestServerGlobalEntries(t, "http://"+address, webRefreshed.Data.AccessToken, fixture.raw, 200, "", "")
		if !reflect.DeepEqual(page.Data, fixture.before.Data) || page.RequestID == fixture.before.RequestID {
			t.Fatal("大小首页、签名或请求 ID 未按预期跨重启保留")
		}
		nextQuery, err := url.ParseQuery(fixture.raw)
		if err != nil {
			t.Fatal("大小重启续读参数错误", err)
		}
		nextQuery.Set("cursor", *fixture.before.Data.NextCursor)
		nextQuery.Set("limit", "100")
		next := requestServerGlobalEntries(t, "http://"+address, webRefreshed.Data.AccessToken, nextQuery.Encode(), 200, "", "")
		if len(next.Data.Items) != len(fixture.nextIDs) || next.Data.NextCursor != nil {
			t.Fatal("大小原游标未跨重启正确结束", len(next.Data.Items), next.Data.NextCursor != nil)
		}
		for index, entryID := range fixture.nextIDs {
			if next.Data.Items[index].ID != entryID {
				t.Fatal("大小原游标跨重启读取条目错误")
			}
		}
	}

	recentFilters := "scope=recent&kind=document&q=" + url.QueryEscape("全局%_")
	recentAfterRestart := requestServerRecentEntries(t, "http://"+address, webRefreshed.Data.AccessToken, recentFilters+"&limit=1", 200, "", "")
	if !reflect.DeepEqual(recentBeforeRestart.Data, recentAfterRestart.Data) || recentBeforeRestart.RequestID == recentAfterRestart.RequestID {
		t.Fatal("最近首页或签名未跨重启保留")
	}
	recentNextAfterRestart := requestServerRecentEntries(t, "http://"+address, webRefreshed.Data.AccessToken, recentFilters+"&cursor="+url.QueryEscape(*recentBeforeRestart.Data.NextCursor), 200, "", "")
	if len(recentNextAfterRestart.Data.Items) != 1 || recentNextAfterRestart.Data.Items[0].ID != "100001" || recentNextAfterRestart.Data.NextCursor != nil {
		t.Fatal("最近普通游标未跨重启续读")
	}
	recentBudgetAfterRestart := requestServerRecentEntries(t, "http://"+address, webRefreshed.Data.AccessToken, "scope=recent&kind=document&q="+url.QueryEscape("预算%_")+"&cursor="+url.QueryEscape(*recentBudgetBeforeRestart.Data.NextCursor), 200, "", "")
	if len(recentBudgetAfterRestart.Data.Items) != 1 || recentBudgetAfterRestart.Data.Items[0].ID != "100008" || recentBudgetAfterRestart.Data.NextCursor != nil {
		t.Fatal("最近预算游标未跨重启续读")
	}
	starredFilters := "scope=starred&kind=document&q=" + url.QueryEscape("全局%_")
	starredListAfterRestart := requestServerGlobalEntries(t, "http://"+address, webRefreshed.Data.AccessToken, starredFilters+"&limit=1", 200, "", "")
	if !reflect.DeepEqual(starredListBeforeRestart.Data, starredListAfterRestart.Data) || starredListBeforeRestart.RequestID == starredListAfterRestart.RequestID {
		t.Fatal("重启后正式收藏首页、签名或请求 ID 错误")
	}
	starredNextAfterRestart := requestServerGlobalEntries(t, "http://"+address, webRefreshed.Data.AccessToken, starredFilters+"&cursor="+url.QueryEscape(*starredListBeforeRestart.Data.NextCursor), 200, "", "")
	if len(starredNextAfterRestart.Data.Items) != 1 || starredNextAfterRestart.Data.Items[0].ID != "100001" || starredNextAfterRestart.Data.NextCursor != nil {
		t.Fatal("重启后原收藏游标续读错误")
	}
	starredBudgetAfterRestart := requestServerGlobalEntries(t, "http://"+address, webRefreshed.Data.AccessToken, "scope=starred&kind=document&q="+url.QueryEscape("预算%_")+"&cursor="+url.QueryEscape(*starredBudgetBeforeRestart.Data.NextCursor), 200, "", "")
	if len(starredBudgetAfterRestart.Data.Items) != 1 || starredBudgetAfterRestart.Data.Items[0].ID != "100008" || starredBudgetAfterRestart.Data.NextCursor != nil {
		t.Fatal("重启后收藏预算空页游标续读错误")
	}
	globalFilters := "scope=all&kind=document&q=" + url.QueryEscape("全局%_")
	globalAfterRestart := requestServerGlobalEntries(t, "http://"+address, webRefreshed.Data.AccessToken, globalFilters+"&limit=1", 200, "", "")
	if !reflect.DeepEqual(globalBeforeRestart.Data, globalAfterRestart.Data) || globalBeforeRestart.RequestID == globalAfterRestart.RequestID {
		t.Fatal("重启后的正式全局首页、签名或请求 ID 错误")
	}
	globalNextAfterRestart := requestServerGlobalEntries(t, "http://"+address, webRefreshed.Data.AccessToken, globalFilters+"&cursor="+url.QueryEscape(*globalBeforeRestart.Data.NextCursor), 200, "", "")
	if len(globalNextAfterRestart.Data.Items) != 1 || globalNextAfterRestart.Data.Items[0].ID != "100001" || globalNextAfterRestart.Data.NextCursor != nil {
		t.Fatal("重启后原全局游标续读错误")
	}
	globalBudgetAfterRestart := requestServerGlobalEntries(t, "http://"+address, webRefreshed.Data.AccessToken, "scope=all&kind=document&q="+url.QueryEscape("预算%_")+"&cursor="+url.QueryEscape(*globalBudgetBeforeRestart.Data.NextCursor), 200, "", "")
	if len(globalBudgetAfterRestart.Data.Items) != 1 || globalBudgetAfterRestart.Data.Items[0].ID != "100008" || globalBudgetAfterRestart.Data.NextCursor != nil {
		t.Fatal("重启后预算空页游标续读错误")
	}
	accessAfterRestart := requestServerRecordAccess(t, "http://"+address, webRefreshed.Data.AccessToken, "90001", `{"purpose":"preview"}`, 200, "", "")
	if !reflect.DeepEqual(accessBeforeRestart.Data, accessAfterRestart.Data) || accessBeforeRestart.RequestID == accessAfterRestart.RequestID {
		t.Fatal("重启后访问时间或当前请求 ID 错误")
	}
	accessDetailAfterRestart := getEntryDetail(t, "90001", webRefreshed.Data.AccessToken, 200, "", "")
	if !reflect.DeepEqual(accessDetailBeforeRestart.Data, accessDetailAfterRestart.Data) {
		t.Fatal("重启后访问详情事实错误")
	}
	starredAfterRestart := requestServerSetStarred(t, "http://"+address, webRefreshed.Data.AccessToken, "80001", `{"starred":true}`, 200, "", "")
	if !reflect.DeepEqual(starredBeforeRestart.Data, starredAfterRestart.Data) || starredBeforeRestart.RequestID == starredAfterRestart.RequestID {
		t.Fatal("重启后收藏时间或当前请求 ID 错误")
	}
	starredDetailAfterRestart := getEntryDetail(t, "80001", webRefreshed.Data.AccessToken, 200, "", "")
	if !reflect.DeepEqual(starredDetailBeforeRestart.Data, starredDetailAfterRestart.Data) {
		t.Fatal("重启后收藏详情事实错误")
	}
	starredPageAfterRestart := listDirectory(t, "parent_id=80002", webRefreshed.Data.AccessToken, 200, "", "")
	if !reflect.DeepEqual(starredPageBeforeRestart.Data, starredPageAfterRestart.Data) {
		t.Fatal("重启后收藏目录列表错误")
	}
	movedEntryAfterRestart := requestServerMoveEntry(t, "http://"+address, webRefreshed.Data.AccessToken, "70001", serverMoveEntryKey, serverMoveEntryBody, 200, "", "")
	if !reflect.DeepEqual(movedEntryBeforeRestart.Data, movedEntryAfterRestart.Data) || movedEntryBeforeRestart.RequestID == movedEntryAfterRestart.RequestID {
		t.Fatal("重启后正式移动快照或当前请求 ID 错误")
	}
	movedDetailAfterRestart := getEntryDetail(t, "70001", webRefreshed.Data.AccessToken, 200, "", "")
	if !reflect.DeepEqual(movedDetailBeforeRestart.Data, movedDetailAfterRestart.Data) {
		t.Fatal("重启后当前位置事实错误")
	}
	movedPageAfterRestart := listDirectory(t, "parent_id=70004", webRefreshed.Data.AccessToken, 200, "", "")
	if !reflect.DeepEqual(movedPageBeforeRestart.Data, movedPageAfterRestart.Data) {
		t.Fatal("重启后移动目的地列表错误")
	}
	renamedEntryAfterRestart := requestServerRenameEntry(t, "http://"+address, webRefreshed.Data.AccessToken, "42", serverRenameEntryKey, serverRenameEntryBody, 200, "", "")
	if !reflect.DeepEqual(renamedEntryBeforeRestart.Data, renamedEntryAfterRestart.Data) || renamedEntryBeforeRestart.RequestID == renamedEntryAfterRestart.RequestID {
		t.Fatal("重启后正式重命名快照或当前请求 ID 错误")
	}
	renamedDetailAfterRestart := getEntryDetail(t, "42", webRefreshed.Data.AccessToken, 200, "", "")
	if !reflect.DeepEqual(renamedDetailBeforeRestart.Data, renamedDetailAfterRestart.Data) {
		t.Fatal("重启后当前改名事实错误")
	}
	createdDirectoryAfterRestart := requestServerCreateDirectory(t, "http://"+address, webRefreshed.Data.AccessToken, serverCreateDirectoryKey, serverCreateDirectoryBody, 201, "", "")
	if !reflect.DeepEqual(createdDirectoryBeforeRestart.Data, createdDirectoryAfterRestart.Data) || createdDirectoryBeforeRestart.RequestID == createdDirectoryAfterRestart.RequestID {
		t.Fatal("重启后正式创建幂等快照或当前请求 ID 错误")
	}
	createdDetailAfterRestart := getEntryDetail(t, createdDirectoryBeforeRestart.Data.Entry.ID, webRefreshed.Data.AccessToken, 200, "", "")
	if !reflect.DeepEqual(createdDetailBeforeRestart.Data, createdDetailAfterRestart.Data) {
		t.Fatal("重启后新建目录的详情事实不一致")
	}
	checkProfile(t)
	detailAfterRestart := getEntryDetail(t, "23", webRefreshed.Data.AccessToken, 200, "", "")
	if !reflect.DeepEqual(detailBeforeRestart.Data, detailAfterRestart.Data) {
		t.Fatal("重启后的目录项详情或偏好不一致")
	}
	directoryAfterRestart := listDirectory(t, "parent_id=12&limit=2", webRefreshed.Data.AccessToken, 200, "", "")
	directoryNextAfterRestart := listDirectory(t, directoryNextQuery, webRefreshed.Data.AccessToken, 200, "", "")
	if !reflect.DeepEqual(directoryBeforeRestart.Data, directoryAfterRestart.Data) || !reflect.DeepEqual(directoryLast.Data, directoryNextAfterRestart.Data) {
		t.Fatal("重启后的目录元数据或原签名游标续页错误")
	}
	devicesAfterRestart := listDevices(t, "", webRefreshed.Data.AccessToken, 200, "", "")
	if !reflect.DeepEqual(devicesBeforeRestart.Data, devicesAfterRestart.Data) {
		t.Fatal("重启前后设备列表事实或当前标记不一致")
	}
	listDevices(t, "", foreignToken.Token, 200, "", "")
	if len(devicesAfterRestart.Data.Items) != 3 || devicesAfterRestart.Data.Items[0].ID != deviceTarget.Data.Device.ID || devicesAfterRestart.Data.Items[0].RevokedAt == nil || devicesAfterRestart.Data.Items[1].Name != nativeDeviceName || devicesAfterRestart.Data.Items[2].Name != webDeviceName {
		t.Fatal("正式设备改名未在重启后持久化")
	}
	if repeatedAt := revokeDevice(t, deviceTarget.Data.Device.ID, webRefreshed.Data.AccessToken, 200, "", ""); !repeatedAt.Equal(deviceRevokedAt) {
		t.Fatal("重启后的设备撤销覆盖首次时间")
	}
	if !reflect.DeepEqual(deviceSessionsBeforeRestart, readDeviceSessions(t, 1, deviceTarget.Data.Device.ID)) {
		t.Fatal("重启或设备撤销重试改变了会话历史时间")
	}
	for _, current := range []dto.LoginResponse{deviceTarget, deviceSibling} {
		request(t, "/api/v1/me", current.Data.AccessToken, 401)
		refresh(t, "native", current.Data.RefreshToken, current.Data.Session.ID, 401, 0)
	}
	if repeatedAt := renameDevice(t, "13", webRefreshed.Data.AccessToken, webDeviceName, 200, "", ""); !repeatedAt.Equal(webRenamedAt) {
		t.Fatal("重启后同名请求覆盖原更新时间")
	}
	if repeatedAt := revoke(t, remoteTarget.Data.Session.ID, webRefreshed.Data.AccessToken, 200, "", ""); !repeatedAt.Equal(remoteRevokedAt) {
		t.Fatal("重启后的重复撤销覆盖首次时间")
	}
	request(t, "/api/v1/me", remoteTarget.Data.AccessToken, 401)
	refresh(t, "native", remoteTarget.Data.RefreshToken, remoteTarget.Data.Session.ID, 401, 0)
	list(t, "", webRefreshed.Data.AccessToken, 200, "", "")
	list(t, "", foreignToken.Token, 200, "", "")
	logout(t, "web", repeatedLogin.Data.AccessToken, repeatedLogin.Data.Session.ID, 401, "SESSION_REVOKED")
	request(t, "/api/v1/me", nativeExited.Data.AccessToken, 401)
	request(t, "/api/v1/me", webLogin.Data.AccessToken, 200)
	request(t, "/api/v1/me", webRefreshed.Data.AccessToken, 200)
	request(t, "/api/v1/me", nativeRefreshed.Data.AccessToken, 200)
	_, webRaw = refresh(t, "web", webRaw, webLogin.Data.Session.ID, 200, 3)
	_, nativeRaw = refresh(t, "native", nativeRaw, nativeLogin.Data.Session.ID, 200, 3)
	if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	request(t, "/api/v1/me", token.Token, 403)
	requestServerSetStarred(t, "http://"+address, token.Token, "80001", `{"starred":true}`, 403, "ACCOUNT_DISABLED", "")
	requestServerRecordAccess(t, "http://"+address, token.Token, "90001", `{"purpose":"open"}`, 403, "ACCOUNT_DISABLED", "")
	requestServerGlobalEntries(t, "http://"+address, token.Token, "scope=starred&limit=0", 403, "ACCOUNT_DISABLED", "")
	requestServerRecentEntries(t, "http://"+address, token.Token, "scope=recent&limit=0", 403, "ACCOUNT_DISABLED", "")
	requestServerGlobalEntries(t, "http://"+address, token.Token, "scope=all&sort=name_asc&limit=0", 403, "ACCOUNT_DISABLED", "")
	requestServerGlobalEntries(t, "http://"+address, token.Token, "scope=all&sort=size_desc&limit=0", 403, "ACCOUNT_DISABLED", "")
	requestServerUploadTask(t, "http://"+address, token.Token, "0", 403, "ACCOUNT_DISABLED", "")
	listDirectory(t, "parent_id=0", token.Token, 403, "ACCOUNT_DISABLED", "")
	getEntryDetail(t, "0", token.Token, 403, "ACCOUNT_DISABLED", "")
	list(t, "", token.Token, 403, "ACCOUNT_DISABLED", "")
	listDevices(t, "", token.Token, 403, "ACCOUNT_DISABLED", "")
	renameDevice(t, "13", token.Token, "失效请求名称", 403, "ACCOUNT_DISABLED", "")
	revoke(t, remoteTarget.Data.Session.ID, token.Token, 403, "ACCOUNT_DISABLED", "")
	revokeDevice(t, deviceTarget.Data.Device.ID, token.Token, 403, "ACCOUNT_DISABLED", "")
	refresh(t, "web", webRaw, webLogin.Data.Session.ID, 403, 0)
	if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 MINUTE) WHERE id=17"); err != nil {
		t.Fatal(err)
	}
	listDirectory(t, directoryNextQuery, token.Token, 401, "SESSION_REVOKED", "")
	requestServerSetStarred(t, "http://"+address, token.Token, "80001", `{"starred":false}`, 401, "SESSION_REVOKED", "")
	requestServerRecordAccess(t, "http://"+address, token.Token, "90001", `{"purpose":"open"}`, 401, "SESSION_REVOKED", "")
	requestServerGlobalEntries(t, "http://"+address, token.Token, "scope=starred&limit=0", 401, "SESSION_REVOKED", "")
	requestServerRecentEntries(t, "http://"+address, token.Token, "scope=recent&limit=0", 401, "SESSION_REVOKED", "")
	requestServerGlobalEntries(t, "http://"+address, token.Token, "scope=all&sort=name_asc&limit=0", 401, "SESSION_REVOKED", "")
	requestServerGlobalEntries(t, "http://"+address, token.Token, "scope=all&sort=size_desc&limit=0", 401, "SESSION_REVOKED", "")
	requestServerUploadTask(t, "http://"+address, token.Token, "500002", 401, "SESSION_REVOKED", "")
	getEntryDetail(t, "23", token.Token, 401, "SESSION_REVOKED", "")
	if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET expires_at=? WHERE id=17", session.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17"); err != nil {
		t.Fatal(err)
	}
	request(t, "/api/v1/me", token.Token, 401)
	requestServerSetStarred(t, "http://"+address, token.Token, "80001", `{"starred":false}`, 401, "SESSION_REVOKED", "")
	requestServerRecordAccess(t, "http://"+address, token.Token, "90001", `{"purpose":"preview"}`, 401, "SESSION_REVOKED", "")
	requestServerGlobalEntries(t, "http://"+address, token.Token, "scope=starred&limit=0", 401, "SESSION_REVOKED", "")
	requestServerRecentEntries(t, "http://"+address, token.Token, "scope=recent&limit=0", 401, "SESSION_REVOKED", "")
	requestServerGlobalEntries(t, "http://"+address, token.Token, "scope=all&sort=name_asc&limit=0", 401, "SESSION_REVOKED", "")
	requestServerGlobalEntries(t, "http://"+address, token.Token, "scope=all&sort=size_desc&limit=0", 401, "SESSION_REVOKED", "")
	requestServerUploadTask(t, "http://"+address, token.Token, "500002", 401, "SESSION_REVOKED", "")
	list(t, "", token.Token, 401, "SESSION_REVOKED", "")
	listDevices(t, "", token.Token, 401, "SESSION_REVOKED", "")
	listDirectory(t, directoryNextQuery, token.Token, 401, "SESSION_REVOKED", "")
	getEntryDetail(t, "23", token.Token, 401, "SESSION_REVOKED", "")
	renameDevice(t, "13", token.Token, "失效请求名称", 401, "SESSION_REVOKED", "")
	revoke(t, remoteTarget.Data.Session.ID, token.Token, 401, "SESSION_REVOKED", "")
	revokeDevice(t, deviceTarget.Data.Device.ID, token.Token, 401, "SESSION_REVOKED", "")
	// 设备撤销后重新登录复用设备 ID，旧会话保持撤销。
	revokeDevice(t, "13", nativeLogin.Data.AccessToken, 200, "", "")
	requestServerSetStarred(t, "http://"+address, webRefreshed.Data.AccessToken, "80001", `{"starred":false}`, 401, "SESSION_REVOKED", "")
	requestServerRecordAccess(t, "http://"+address, webRefreshed.Data.AccessToken, "90001", `{"purpose":"preview"}`, 401, "SESSION_REVOKED", "")
	requestServerGlobalEntries(t, "http://"+address, webRefreshed.Data.AccessToken, "scope=starred&limit=0", 401, "SESSION_REVOKED", "")
	requestServerRecentEntries(t, "http://"+address, webRefreshed.Data.AccessToken, "scope=recent&limit=0", 401, "SESSION_REVOKED", "")
	requestServerGlobalEntries(t, "http://"+address, webRefreshed.Data.AccessToken, "scope=all&sort=name_asc&limit=0", 401, "SESSION_REVOKED", "")
	requestServerGlobalEntries(t, "http://"+address, webRefreshed.Data.AccessToken, "scope=all&sort=size_desc&limit=0", 401, "SESSION_REVOKED", "")
	requestServerUploadTask(t, "http://"+address, webRefreshed.Data.AccessToken, "500002", 401, "SESSION_REVOKED", "")
	request(t, "/api/v1/me", webLogin.Data.AccessToken, 401)
	list(t, "", webLogin.Data.AccessToken, 401, "SESSION_REVOKED", "")
	listDevices(t, "", webLogin.Data.AccessToken, 401, "SESSION_REVOKED", "")
	listDirectory(t, directoryNextQuery, webLogin.Data.AccessToken, 401, "SESSION_REVOKED", "")
	getEntryDetail(t, "23", webLogin.Data.AccessToken, 401, "SESSION_REVOKED", "")
	renameDevice(t, "13", webLogin.Data.AccessToken, "失效请求名称", 401, "SESSION_REVOKED", "")
	revoke(t, remoteTarget.Data.Session.ID, webLogin.Data.AccessToken, 401, "SESSION_REVOKED", "")
	revokeDevice(t, deviceTarget.Data.Device.ID, webLogin.Data.AccessToken, 401, "SESSION_REVOKED", "")
	list(t, "", nativeLogin.Data.AccessToken, 200, "", "")
	listDirectory(t, directoryNextQuery, nativeLogin.Data.AccessToken, 200, "", "")
	getEntryDetail(t, "23", nativeLogin.Data.AccessToken, 200, "", "")
	revokedDevices := listDevices(t, "", nativeLogin.Data.AccessToken, 200, "", "")
	if len(revokedDevices.Data.Items) != 3 || revokedDevices.Data.Items[2].ID != "13" || revokedDevices.Data.Items[2].RevokedAt == nil {
		t.Fatal("有效设备未看到其他设备的撤销事实")
	}
	webDeviceName = "撤销后重新命名的浏览器"
	renameDevice(t, "13", nativeLogin.Data.AccessToken, webDeviceName, 200, "", "")
	refresh(t, "web", webRaw, webLogin.Data.Session.ID, 401, 0)
	restoredLogin, restoredCookies := login(t, "web", webInstallation, "fixture-password", 200)
	if restoredLogin.Data.Device.ID != "13" || restoredLogin.Data.Device.Name != webDeviceName {
		t.Fatal("撤销设备重新登录没有复用 ID")
	}
	listDirectory(t, directoryNextQuery, restoredLogin.Data.AccessToken, 200, "", "")
	getEntryDetail(t, "23", restoredLogin.Data.AccessToken, 200, "", "")
	listDirectory(t, directoryNextQuery, webLogin.Data.AccessToken, 401, "SESSION_REVOKED", "")
	restoredDevices := listDevices(t, "", restoredLogin.Data.AccessToken, 200, "", "")
	if len(restoredDevices.Data.Items) != 3 || restoredDevices.Data.Items[2].ID != "13" || restoredDevices.Data.Items[2].RevokedAt != nil || !restoredDevices.Data.Items[2].Current {
		t.Fatal("恢复设备的 ID、撤销状态或当前标记错误")
	}
	request(t, "/api/v1/me", webLogin.Data.AccessToken, 401)
	request(t, "/api/v1/me", nativeLogin.Data.AccessToken, 200)
	refresh(t, "web", webRaw, webLogin.Data.Session.ID, 401, 0)
	restoredRefreshed, restoredRaw := refresh(t, "web", restoredCookies[0].Value, restoredLogin.Data.Session.ID, 200, 2)
	nativeFinal, nativeFinalRaw := refresh(t, "native", nativeRaw, nativeLogin.Data.Session.ID, 200, 4)
	// 指定当前会话提交后，其旧访问与刷新凭证均失效，其他会话仍可请求。
	selfTarget, _ := login(t, "native", nativeInstallation, "fixture-password", 200)
	revoke(t, selfTarget.Data.Session.ID, selfTarget.Data.AccessToken, 200, "", "")
	request(t, "/api/v1/me", selfTarget.Data.AccessToken, 401)
	refresh(t, "native", selfTarget.Data.RefreshToken, selfTarget.Data.Session.ID, 401, 0)
	revoke(t, remoteTarget.Data.Session.ID, selfTarget.Data.AccessToken, 401, "SESSION_REVOKED", "")
	request(t, "/api/v1/me", nativeFinal.Data.AccessToken, 200)
	revoke(t, selfTarget.Data.Session.ID, nativeFinal.Data.AccessToken, 200, "", "")
	// 正式重新登录恢复原目标设备，然后撤销当前设备及其全部新会话。
	deviceSelf, _ := login(t, "native", remoteInstallation, "fixture-password", 200)
	deviceSelfSibling, _ := login(t, "native", remoteInstallation, "fixture-password", 200)
	if deviceSelf.Data.Device.ID != deviceTarget.Data.Device.ID || deviceSelfSibling.Data.Device.ID != deviceTarget.Data.Device.ID || deviceSelf.Data.Session.ID == deviceTarget.Data.Session.ID {
		t.Fatal("正式重新登录未复用设备并创建新会话")
	}
	request(t, "/api/v1/me", deviceTarget.Data.AccessToken, 401)
	request(t, "/api/v1/me", deviceSibling.Data.AccessToken, 401)
	refresh(t, "native", deviceTarget.Data.RefreshToken, deviceTarget.Data.Session.ID, 401, 0)
	selfDeviceRevokedAt := revokeDevice(t, deviceSelf.Data.Device.ID, deviceSelf.Data.AccessToken, 200, "", "")
	for _, current := range []dto.LoginResponse{deviceSelf, deviceSelfSibling} {
		request(t, "/api/v1/me", current.Data.AccessToken, 401)
		refresh(t, "native", current.Data.RefreshToken, current.Data.Session.ID, 401, 0)
		revokeDevice(t, "13", current.Data.AccessToken, 401, "SESSION_REVOKED", "")
	}
	if repeatedAt := revokeDevice(t, deviceSelf.Data.Device.ID, nativeFinal.Data.AccessToken, 200, "", ""); !repeatedAt.Equal(selfDeviceRevokedAt) {
		t.Fatal("其他有效设备重复撤销改变了首次时间")
	}
	request(t, "/api/v1/me", nativeFinal.Data.AccessToken, 200)
	for attempt := 1; attempt <= 11; attempt++ {
		status := 401
		if attempt == 11 {
			status = 429
		}
		login(t, "web", webInstallation, "wrong-password", status)
	}
	request(t, "/api/v1/me", restoredLogin.Data.AccessToken, 200)
	list(t, "", restoredLogin.Data.AccessToken, 200, "", "")
	listDevices(t, "", restoredLogin.Data.AccessToken, 200, "", "")
	// 第二轮启动沿用 Secure 配置，刷新后的最新凭证也受当前会话撤销控制。
	logout(t, "web", restoredRefreshed.Data.AccessToken, restoredLogin.Data.Session.ID, 200, "")
	request(t, "/api/v1/me", restoredLogin.Data.AccessToken, 401)
	request(t, "/api/v1/me", restoredRefreshed.Data.AccessToken, 401)
	refresh(t, "web", restoredRaw, restoredLogin.Data.Session.ID, 401, 0)
	logout(t, "web", restoredRefreshed.Data.AccessToken, restoredLogin.Data.Session.ID, 401, "SESSION_REVOKED")
	list(t, "", restoredRefreshed.Data.AccessToken, 401, "SESSION_REVOKED", "")
	listDevices(t, "", restoredRefreshed.Data.AccessToken, 401, "SESSION_REVOKED", "")
	listDirectory(t, directoryNextQuery, restoredRefreshed.Data.AccessToken, 401, "SESSION_REVOKED", "")
	getEntryDetail(t, "23", restoredRefreshed.Data.AccessToken, 401, "SESSION_REVOKED", "")
	renameDevice(t, "13", restoredRefreshed.Data.AccessToken, "退出后名称", 401, "SESSION_REVOKED", "")
	revokeDevice(t, deviceTarget.Data.Device.ID, restoredRefreshed.Data.AccessToken, 401, "SESSION_REVOKED", "")
	list(t, "", nativeFinal.Data.AccessToken, 200, "", "")
	request(t, "/api/v1/me", nativeFinal.Data.AccessToken, 200)
	logout(t, "native", nativeFinal.Data.AccessToken, nativeLogin.Data.Session.ID, 200, "")
	request(t, "/api/v1/me", nativeLogin.Data.AccessToken, 401)
	request(t, "/api/v1/me", nativeFinal.Data.AccessToken, 401)
	refresh(t, "native", nativeFinalRaw, nativeLogin.Data.Session.ID, 401, 0)
	list(t, "", nativeFinal.Data.AccessToken, 401, "SESSION_REVOKED", "")
	listDevices(t, "", nativeFinal.Data.AccessToken, 401, "SESSION_REVOKED", "")
	listDirectory(t, directoryNextQuery, nativeFinal.Data.AccessToken, 401, "SESSION_REVOKED", "")
	getEntryDetail(t, "23", nativeFinal.Data.AccessToken, 401, "SESSION_REVOKED", "")
	renameDevice(t, nativeLogin.Data.Device.ID, nativeFinal.Data.AccessToken, "退出后名称", 401, "SESSION_REVOKED", "")
	revokeDevice(t, deviceTarget.Data.Device.ID, nativeFinal.Data.AccessToken, 401, "SESSION_REVOKED", "")
	list(t, "", foreignToken.Token, 200, "", "")
	stopSecond()
	uploadEntries, err = os.ReadDir(uploadDirectory)
	uploadContent, uploadReadError := os.ReadFile(uploadNeighbor)
	if err != nil || len(uploadEntries) != 1 || uploadReadError != nil || string(uploadContent) != "keep" {
		t.Fatal("重启临时目录检查留下探测文件或改变已有内容", err, uploadReadError)
	}
	startupSegments := strings.Split(logs.String(), "准备启动 HTTP 服务")
	if len(startupSegments) != 3 || strings.Count(logs.String(), "上传临时目录检查通过") != 2 || !strings.Contains(startupSegments[0], "上传临时目录检查通过") || !strings.Contains(startupSegments[1], "上传临时目录检查通过") {
		t.Fatal("两轮临时目录检查未先于 HTTP 启动")
	}
	workerStops := strings.Split(logs.String(), "上传校验 worker 已停止")
	if len(workerStops) != 3 || strings.Count(logs.String(), "上传校验 worker 已启动") != 2 {
		t.Fatal("每次真实启动应对应一个校验 worker 的启动和停止")
	}
	for _, afterWorkerStop := range workerStops[1:] {
		storageClosed := strings.Index(afterWorkerStop, "MinIO 空闲连接已释放")
		databaseClosed := strings.Index(afterWorkerStop, "MySQL 连接池已关闭")
		if storageClosed < 0 || databaseClosed <= storageClosed {
			t.Fatal("校验 worker 退出前已清理依赖，或连接关闭顺序错误")
		}
	}
	after, err := os.Stat(secretPath)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("生产启动流程重写了已有密钥")
	}
	if strings.Contains(logs.String(), token.Token) || strings.Contains(logs.String(), "fixture-refresh") || strings.Contains(logs.String(), "private-query") {
		t.Fatal("启动或访问日志泄漏了凭证")
	}
	for _, secret := range loginSecrets {
		if strings.Contains(logs.String(), secret) {
			t.Error("正式认证日志泄漏凭证")
		}
	}
	for _, privateValue := range []string{"private-upload-create-query", "private-upload-create-cookie", "private-invalid-sha", serverCreateUploadKey, "server-upload-", "正式上传 🌱.bin", "private-upload-query", "private-upload-cookie", "private-upload-diagnostic", "upload-route-", "private-size-cursor", "size-budget-", "size-tie-", "size-zero", "private-name-cursor", "name-budget-", "name-long-", "private-recent-query", "private-recent-cursor", "最近专用未访问", "private-starred-cursor", "private-global-query", "private-global-cursor", "全局%_", "预算%_", "private-directory-query", "private-directory-cursor", "private-detail-query", "private-create-query", "private-rename-query", "private-move-query", "private-star-query", "private-access-query", serverCreateDirectoryKey, serverRenameEntryKey, serverMoveEntryKey, "正式创建 🌱", "正式笔记.PDF", "移动文件.md", "正式移动来源", "收藏文件.md", "正式收藏来源", "访问文件.md", "首次访问.md", "正式访问来源", "学习笔记.md", "发布时设备 🎮"} {
		if strings.Contains(logs.String(), privateValue) {
			t.Error("正式目录完成日志泄漏原始查询或游标")
		}
	}
}
