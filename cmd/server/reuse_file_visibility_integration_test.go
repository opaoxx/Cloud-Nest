package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/repository"
	"golang.org/x/crypto/bcrypt"
)

func TestServerReuseFileVisibilityIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_REUSE_FILE_VISIBILITY_TEST") != "1" {
		t.Skip("显式启用后启动独占 MySQL、MinIO 和回环服务")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	// 1. 启动正式服务，为两个合成账号建立独立的目录、会话与权限范围。
	fixture := prepareBackupSource(t, ctx)
	t.Logf("秒传专用 MySQL %s，专用 MinIO %s", fixture.mysqlContainer[:12], fixture.minioContainer[:12])
	other, otherLogin := loginReuseVisibilityAccount(t, ctx, fixture)
	store, err := repository.NewStore(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	var folder dto.CreateDirectoryResponse
	fixture.api.request(t, ctx, http.MethodPost, "/api/v1/directories", "reuse-visibility-folder",
		strings.NewReader(fmt.Sprintf(`{"parent_id":%q,"name":"待回收的秒传夹具"}`, fixture.login.RootEntryID)), http.StatusCreated, &folder)
	ownerID := backupFixtureID(t, fixture.login.User.ID)
	folderID := backupFixtureID(t, folder.Data.Entry.ID)
	sourceBytes := []byte{0, 19, 42, 128, 255}
	sourceID, object := publishBackupSourceFile(t, ctx, fixture.api, store, fixture.bucket, ownerID, folderID, 1, "隔离可见性.bin", sourceBytes)
	sourceDigest := sha256.Sum256(sourceBytes)

	assertUnavailable := func(api backupSourceClient, rootID, key string, digest [sha256.Size]byte) {
		t.Helper()
		before := readWebPurgePreviewSnapshot(t, ctx, fixture.pool)
		body := fmt.Sprintf(`{"parent_id":%q,"name":"复用尝试.bin","size_bytes":%d,"sha256":"%x"}`, rootID, len(sourceBytes), digest)
		var raw json.RawMessage
		api.request(t, ctx, http.MethodPost, "/api/v1/files/reuse", key, strings.NewReader(body), http.StatusNotFound, &raw)
		var result dto.ErrorResponse
		if err := json.Unmarshal(raw, &result); err != nil || result.RequestID == "" {
			t.Fatal("未命中响应缺少请求标识或无效", err)
		}
		want, err := json.Marshal(dto.ErrorResponse{
			Error: dto.ErrorDetail{Code: "REUSABLE_CONTENT_NOT_FOUND", Message: "没有可复用的内容"}, RequestID: result.RequestID,
		})
		if err != nil || !bytes.Equal(want, raw) {
			t.Fatalf("未命中响应未保持固定公开字段，得到 %s 和 %v", raw, err)
		}
		if after := readWebPurgePreviewSnapshot(t, ctx, fixture.pool); !reflect.DeepEqual(after, before) {
			t.Fatal("拒绝秒传后目录项、对象、配额或幂等记录发生变化")
		}
	}

	// 2. 另一账号使用自己的根目录，比较外部内容和完全不存在的内容所获响应。
	assertUnavailable(other, otherLogin.RootEntryID, "reuse-visibility-other", sourceDigest)
	missingDigest := sha256.Sum256([]byte{0, 19, 42, 128, 254})
	assertUnavailable(other, otherLogin.RootEntryID, "reuse-visibility-missing", missingDigest)

	// 3. 正式回收源文件的父目录，子文件和对象仍存在，但完整路径不再可见。
	var trashed dto.TrashEntryResponse
	fixture.api.request(t, ctx, http.MethodPost, "/api/v1/entries/"+folder.Data.Entry.ID+"/trash", "reuse-visibility-trash",
		strings.NewReader(fmt.Sprintf(`{"expected_revision":%q}`, folder.Data.Entry.Revision)), http.StatusOK, &trashed)
	if trashed.Data.Entry.ID != folder.Data.Entry.ID || trashed.Data.Entry.Status != "TRASHED" {
		t.Fatal("源目录没有进入回收站")
	}
	source, err := store.FindUserFileEntry(ctx, ownerID, sourceID)
	if err != nil || source == nil || source.Status != "ACTIVE" || source.ParentID == nil || *source.ParentID != folderID {
		t.Fatal("源文件未保留在回收祖先下", err)
	}
	beforeReuse, err := store.FindUserStoredObject(ctx, ownerID, object.ID)
	if err != nil || beforeReuse == nil || beforeReuse.Status != domain.StoredObjectStatusReady {
		t.Fatal("源对象没有保持 READY", err)
	}
	assertUnavailable(fixture.api, fixture.login.RootEntryID, "reuse-visibility-hidden", sourceDigest)
	t.Log("跨账号、无匹配与回收祖先均统一返回 404，目录项、对象、配额与幂等记录未改变")
}

func loginReuseVisibilityAccount(t *testing.T, ctx context.Context, fixture backupSourceFixture) (backupSourceClient, dto.LoginData) {
	t.Helper()
	// 1. 在本轮专用库创建第二个账号及真实根目录。
	const username = "reuse_visibility_other"
	password := rand.Text()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	account, err := fixture.pool.ExecContext(ctx, "INSERT INTO users(username,display_name,password_hash) VALUES(?,?,?)",
		username, "秒传隔离对照账号", string(hash))
	if err != nil {
		t.Fatal(err)
	}
	userID, err := account.LastInsertId()
	if err != nil || userID <= 0 {
		t.Fatal("未取得隔离对照账号标识", err)
	}
	if _, err := fixture.pool.ExecContext(ctx, "INSERT INTO file_entries(user_id,name,name_key,entry_type) VALUES(?,'',X'','DIR')", userID); err != nil {
		t.Fatal(err)
	}

	// 2. 通过真实登录取得该账号的访问令牌与根目录标识。
	body, err := json.Marshal(dto.LoginRequest{Username: username, Password: password, DeviceName: "秒传隔离设备",
		InstallationID: uuid.NewString(), ClientType: "native", Platform: "WINDOWS"})
	if err != nil {
		t.Fatal(err)
	}
	other := fixture.api
	other.accessToken = ""
	var response dto.LoginResponse
	other.request(t, ctx, http.MethodPost, "/api/v1/auth/login", "", bytes.NewReader(body), http.StatusOK, &response)
	if response.Data.User.ID != strconv.FormatInt(userID, 10) || response.Data.RootEntryID == "" ||
		response.Data.RootEntryID == fixture.login.RootEntryID || response.Data.AccessToken == "" {
		t.Fatal("对照账号登录没有返回自己的身份和根目录")
	}
	other.accessToken = response.Data.AccessToken
	return other, response.Data
}
