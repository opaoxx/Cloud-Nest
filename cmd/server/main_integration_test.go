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
	"os"
	"path/filepath"
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
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
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
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR')",
		"INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(13,1,'28ee758a-0727-42de-ae7d-3bb33155e148','启动测试设备','WEB')",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	session := domain.LoginSession{ID: 17, UserID: 1, DeviceID: 13, ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
	hash := sha256.Sum256([]byte("fixture-refresh"))
	if _, err := pool.ExecContext(ctx, "INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES(17,1,13,?,?)", hash[:], session.ExpiresAt); err != nil {
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

	// 3. 重启后沿用原 JWT，再验证数据库禁用及会话撤销立即影响后续请求。
	secretPath := filepath.Join(configurationDirectory, "CloudNest", "jwt-secret")
	before, err := os.Stat(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	stopFirst()
	stopSecond := start()
	checkProfile(t)
	if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	request(t, "/api/v1/me", token.Token, 403)
	if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17"); err != nil {
		t.Fatal(err)
	}
	request(t, "/api/v1/me", token.Token, 401)
	stopSecond()
	after, err := os.Stat(secretPath)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("生产启动流程重写了已有密钥")
	}
	if strings.Contains(logs.String(), token.Token) || strings.Contains(logs.String(), "fixture-refresh") {
		t.Fatal("启动或访问日志泄漏了凭证")
	}
}
