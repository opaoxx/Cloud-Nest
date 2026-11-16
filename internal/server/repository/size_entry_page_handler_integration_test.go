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

func TestSizeEntryPageHandlerIntegration(t *testing.T) {
	// 1. 在独占库使用正式表准备跨目录、隐藏路径及其他用户的数据。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	if start < 0 {
		t.Fatal("未找到正式会话表")
	}
	end := strings.Index(migrations.InitialSQL[start:], "CREATE TABLE stored_objects (")
	if end < 0 {
		t.Fatal("未找到正式会话表结束边界")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[start:start+end]); err != nil {
		t.Fatal(err)
	}
	preferencesStart := strings.Index(migrations.InitialSQL, "CREATE TABLE user_entry_preferences (")
	if preferencesStart < 0 {
		t.Fatal("未找到正式偏好表")
	}
	preferencesEnd := strings.Index(migrations.InitialSQL[preferencesStart:], "CREATE TABLE upload_tasks (")
	if preferencesEnd < 0 {
		t.Fatal("未找到正式偏好表结束边界")
	}
	if _, err := pool.ExecContext(ctx, migrations.InitialSQL[preferencesStart:preferencesStart+preferencesEnd]); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','大小处理用户','fixture_hash'),(3,'fixture_three','其他用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-size-handler',2048),(43,3,'MINIO','fixture-bucket','fixture-size-foreign',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'目录一',CAST('目录一' AS BINARY),'DIR'),(14,1,11,'目录二',CAST('目录二' AS BINARY),'DIR'),(24,1,12,'学习%_目录',CAST('学习%_目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(18,1,11,'回收目录',CAST('回收目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(13,1,12,'学习%_笔记.md',CAST('学习%_笔记.md' AS BINARY),'FILE','DOCUMENT',41,4096),(15,1,14,'学习%_计划.md',CAST('学习%_计划.md' AS BINARY),'FILE','DOCUMENT',41,2048),(17,1,18,'学习%_隐藏.md',CAST('学习%_隐藏.md' AS BINARY),'FILE','DOCUMENT',41,2048),(35,3,33,'学习%_其他.md',CAST('学习%_其他.md' AS BINARY),'FILE','DOCUMENT',43,2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(19,1,12,'empty-size.bin',CAST('empty-size.bin' AS BINARY),'FILE','OTHER',41,0)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=?", time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	// 2. 创建真实设备/会话并签发 JWT，组合授权、快照和游标分页服务。
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
	newLogin := func(userID uint64) (string, domain.Actor) {
		t.Helper()
		_, hash := service.GenerateRefreshToken()
		var session *domain.LoginSession
		if err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, uuid.NewString(), "大小 HTTP 测试", "WEB")
			if err != nil {
				return err
			}
			session, err = txStore.CreateLoginSession(txContext, device.ID, hash, time.Now().Add(time.Hour))
			return err
		}); err != nil || session == nil {
			t.Fatal("创建大小认证夹具失败", err)
		}
		token, err := tokenSigner.Issue(*session)
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, token.Token)
		return token.Token, domain.Actor{UserID: userID, SessionID: session.ID, DeviceID: session.DeviceID}
	}
	withSnapshot := func(current context.Context, action func(context.Context, service.SizeEntryListReader) error) error {
		return store.WithFileEntrySnapshot(current, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
	}
	lists := service.NewSizeEntryListService(withSnapshot)
	cursorSigner, err := service.NewSizeEntryCursorSigner(configuration.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	cursorVerifier, err := service.NewSizeEntryCursorVerifier(configuration.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	pages := service.NewSizeEntryPageService(cursorVerifier.Verify, lists.List, cursorSigner.Sign)
	failingLists := service.NewSizeEntryListService(func(current context.Context, action func(context.Context, service.SizeEntryListReader) error) error {
		return withSnapshot(current, func(snapshotContext context.Context, reader service.SizeEntryListReader) error {
			if err := action(snapshotContext, reader); err != nil {
				return err
			}
			return errors.New("private-snapshot-failure")
		})
	})
	failingPages := service.NewSizeEntryPageService(cursorVerifier.Verify, failingLists.List, cursorSigner.Sign)
	type modeKey struct{}
	var calls atomic.Int32
	var logs bytes.Buffer
	router := serverhttp.NewRouter(logging.New(&logs), []string{"http://127.0.0.1:5173"}, func(context.Context) bool { return true })
	router.GET("/api/v1/test-size-entry-pages", middleware.RequireAuthorization(authorization.Authorize, handler.AuthorizationFailure), func(requestContext *gin.Context) {
		requestContext.Request = requestContext.Request.WithContext(context.WithValue(requestContext.Request.Context(), modeKey{}, requestContext.GetHeader("X-Test-Mode")))
	}, handler.SizeEntryPage(func(current context.Context, actor domain.Actor, query domain.GlobalEntryQuery, cursor string) (*domain.SizeEntryPageResult, error) {
		calls.Add(1)
		switch current.Value(modeKey{}) {
		case "snapshot-failure":
			return failingPages.List(current, actor, query, cursor)
		case "disable-after-auth":
			if _, err := pool.ExecContext(current, "UPDATE users SET status='DISABLED' WHERE id=?", actor.UserID); err != nil {
				return nil, err
			}
		}
		return pages.List(current, actor, query, cursor)
	}))
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	request := func(token, raw, mode string, status, serviceCalls int, code, field string) dto.SizeEntryPageResponse {
		t.Helper()
		before := calls.Load()
		requestID := uuid.NewString()
		message, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v1/test-size-entry-pages?"+raw+"&user_id=3&parent_id=private-parent&note=private-query", nil)
		if err != nil {
			t.Fatal(err)
		}
		message.Header.Set("Origin", "http://127.0.0.1:5173")
		message.Header.Set("X-Request-ID", strings.ToUpper(requestID))
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
		if readError != nil || closeError != nil || response.StatusCode != status || int(calls.Load()-before) != serviceCalls || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Content-Encoding") != "" || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || len(response.Cookies()) != 0 {
			t.Fatal("真实大小请求状态、调用或响应头错误", response.StatusCode, calls.Load()-before, readError, closeError)
		}
		var result dto.SizeEntryPageResponse
		if status == 200 {
			if json.Unmarshal(encoded, &result) != nil || result.RequestID != requestID || result.Data.Items == nil || result.Data.Breadcrumbs == nil || len(result.Data.Breadcrumbs) != 0 || response.Header.Get("Pragma") != "no-cache" || response.ContentLength != int64(len(encoded)) {
				t.Fatal("真实大小成功 JSON、缓存或长度错误")
			}
			if result.Data.NextCursor != nil {
				secrets = append(secrets, *result.Data.NextCursor)
			}
			return result
		}
		var body dto.ErrorResponse
		if json.Unmarshal(encoded, &body) != nil || body.Error.Code != code || body.RequestID != requestID || strings.Contains(string(encoded), "private-") {
			t.Fatal("真实大小错误结构或原文保护错误")
		}
		if field == "" {
			if body.Error.Details != nil {
				t.Fatal("非字段错误 details 非空")
			}
		} else if len(body.Error.Details) != 1 || body.Error.Details["field"] != field {
			t.Fatal("真实大小错误字段错误")
		}
		if (status == 401) != (response.Header.Get("WWW-Authenticate") != "") {
			t.Fatal("真实大小认证提示与状态不匹配")
		}
		return result
	}
	caller, _ := newLogin(1)
	foreign, _ := newLogin(3)
	filters := "scope=all&kind=document&q=" + url.QueryEscape("学习%_")

	// 3. 真实 GET 完成跨目录分页、默认查询、类型交集和用户隔离。
	first := request(caller, filters+"&limit=1", "", 200, 1, "", "")
	if len(first.Data.Items) != 1 || first.Data.Items[0].ID != "13" || first.Data.Items[0].ParentID != "12" || first.Data.NextCursor == nil {
		t.Fatal("真实大小首页或原文筛选错误")
	}
	next := request(caller, filters+"&limit=2&cursor="+url.QueryEscape(*first.Data.NextCursor), "", 200, 1, "", "")
	if len(next.Data.Items) != 1 || next.Data.Items[0].ID != "15" || next.Data.Items[0].ParentID != "14" || next.Data.NextCursor != nil {
		t.Fatal("真实跨目录续页或页大小调整错误")
	}
	all := request(caller, "", "", 200, 1, "", "")
	if len(all.Data.Items) != 6 || all.Data.Items[0].ID != "13" || all.Data.NextCursor != nil {
		t.Fatal("默认大小范围、根排除或隐藏过滤错误")
	}
	folders := request(caller, "kind=folder&q="+url.QueryEscape("学习%_"), "", 200, 1, "", "")
	if len(folders.Data.Items) != 1 || folders.Data.Items[0].ID != "24" || folders.Data.NextCursor != nil {
		t.Fatal("文件夹与搜索交集错误")
	}
	empty := request(caller, "kind=image", "", 200, 1, "", "")
	if len(empty.Data.Items) != 0 || empty.Data.NextCursor != nil {
		t.Fatal("真实空结束页错误")
	}
	zero := request(caller, "kind=other", "", 200, 1, "", "")
	if len(zero.Data.Items) != 1 || zero.Data.Items[0].ID != "19" || zero.Data.Items[0].EntryType != "FILE" || zero.Data.Items[0].SizeBytes != 0 || zero.Data.NextCursor != nil {
		t.Fatal("真实零字节文件结果错误")
	}
	other := request(foreign, "", "", 200, 1, "", "")
	if len(other.Data.Items) != 1 || other.Data.Items[0].ID != "35" {
		t.Fatal("真实用户范围隔离错误")
	}

	starred := request(caller, "scope=starred", "", 200, 1, "", "")
	if len(starred.Data.Items) != 0 || starred.Data.NextCursor != nil {
		t.Fatal("当前支持的收藏范围未返回空结束页")
	}
	// 4. 组合真实收藏成员和跨目录同大小文件，检查大小与 ID 双降序。
	if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) VALUES(1,13,UTC_TIMESTAMP(6)),(1,15,UTC_TIMESTAMP(6))"); err != nil {
		t.Fatal(err)
	}
	starredFirst := request(caller, "scope=starred&limit=1", "", 200, 1, "", "")
	if len(starredFirst.Data.Items) != 1 || starredFirst.Data.Items[0].ID != "13" || starredFirst.Data.NextCursor == nil {
		t.Fatal("真实大小收藏首页顺序错误")
	}
	starredNext := request(caller, "scope=starred&limit=2&cursor="+url.QueryEscape(*starredFirst.Data.NextCursor), "", 200, 1, "", "")
	if len(starredNext.Data.Items) != 1 || starredNext.Data.Items[0].ID != "15" || starredNext.Data.NextCursor != nil {
		t.Fatal("真实大小收藏续页错误")
	}
	request(caller, "scope=all&cursor="+url.QueryEscape(*starredFirst.Data.NextCursor), "", 400, 1, "INVALID_CURSOR", "cursor")
	for _, entry := range []struct{ entryID, parentID uint64 }{{501, 12}, {502, 14}} {
		name := fmt.Sprintf("size-tie-%d.md", entry.entryID)
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,?,?,?,'FILE','DOCUMENT',41,8192)", entry.entryID, entry.parentID, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) VALUES(1,?,UTC_TIMESTAMP(6))", entry.entryID); err != nil {
			t.Fatal(err)
		}
	}
	for _, scope := range []string{"all", "starred"} {
		query := "scope=" + scope + "&q=size-tie-&kind=document&limit=1"
		firstTie := request(caller, query, "", 200, 1, "", "")
		if len(firstTie.Data.Items) != 1 || firstTie.Data.Items[0].ID != "502" || firstTie.Data.Items[0].SizeBytes != 8192 || firstTie.Data.NextCursor == nil || len(*firstTie.Data.NextCursor) > domain.MaxSizeEntryCursorLength {
			t.Fatal("真实大小同值首页错误")
		}
		nextTie := request(caller, query+"&cursor="+url.QueryEscape(*firstTie.Data.NextCursor), "", 200, 1, "", "")
		if len(nextTie.Data.Items) != 1 || nextTie.Data.Items[0].ID != "501" || nextTie.Data.NextCursor != nil {
			t.Fatal("真实大小同值续页错误")
		}
	}

	// 5. 创建隐藏预算候选，用处理函数完成空页和短页续读。
	writer, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	for index := 0; index < 1001; index++ {
		name := fmt.Sprintf("blocked-%04d.md", index)
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,18,?,?,'FILE','DOCUMENT',41,2048)", 3000+index, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct {
		entryID, parentID, size uint64
		name                    string
	}{{2901, 12, 1024, "blocked-z-visible.md"}, {2903, 14, 512, "blocked-zz-visible.md"}} {
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,?,?,?,'FILE','DOCUMENT',41,?)", entry.entryID, entry.parentID, entry.name, []byte(entry.name), entry.size); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) SELECT 1,id,UTC_TIMESTAMP(6) FROM file_entries WHERE user_id=1 AND (id IN (2901,2903) OR id BETWEEN 3000 AND 4000)"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"all", "starred"} {
		query := "scope=" + scope + "&q=blocked-&kind=document&limit=2"
		budget := request(caller, query, "", 200, 1, "", "")
		if len(budget.Data.Items) != 0 || budget.Data.NextCursor == nil {
			t.Fatal("真实大小预算空页错误")
		}
		position, err := cursorVerifier.Verify(1, domain.GlobalEntryQuery{Scope: scope, Sort: "size_desc", Search: "blocked-", Kind: "document", PageSize: 2}, *budget.Data.NextCursor)
		if err != nil || position == nil || position.ID != 3001 || position.FileSize != 2048 {
			t.Fatal("真实大小预算最后消费位置错误", err)
		}
		next := request(caller, query+"&cursor="+url.QueryEscape(*budget.Data.NextCursor), "", 200, 1, "", "")
		if len(next.Data.Items) != 2 || next.Data.Items[0].ID != "2901" || next.Data.Items[1].ID != "2903" || next.Data.NextCursor != nil {
			t.Fatal("真实大小预算空页续读错误")
		}
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=12 WHERE id=4000"); err != nil {
		t.Fatal(err)
	}
	short := request(caller, "q=blocked-&limit=3", "", 200, 1, "", "")
	if len(short.Data.Items) != 1 || short.Data.Items[0].ID != "4000" || short.Data.NextCursor == nil {
		t.Fatal("真实大小预算短页错误")
	}
	shortNext := request(caller, "q=blocked-&limit=3&cursor="+url.QueryEscape(*short.Data.NextCursor), "", 200, 1, "", "")
	if len(shortNext.Data.Items) != 2 || shortNext.Data.Items[0].ID != "2901" || shortNext.Data.Items[1].ID != "2903" || shortNext.Data.NextCursor != nil {
		t.Fatal("真实大小预算短页续读错误")
	}

	// 6. 验证认证先行、参数定位及游标的用户/筛选/用途绑定。
	request("", "limit=0", "", 401, 0, "AUTH_REQUIRED", "")
	request(caller, "limit=0", "", 400, 0, "INVALID_ARGUMENT", "limit")
	request(caller, "scope=recent", "", 400, 0, "INVALID_ARGUMENT", "scope")
	request(caller, "q=%FF", "", 400, 0, "INVALID_ARGUMENT", "q")
	request(caller, "note=%ZZ", "", 400, 0, "INVALID_ARGUMENT", "query")
	request(caller, "cursor=a&cursor=b", "", 400, 0, "INVALID_ARGUMENT", "cursor")
	request(caller, "cursor=private-invalid-cursor", "", 400, 1, "INVALID_CURSOR", "cursor")
	request(foreign, filters+"&cursor="+url.QueryEscape(*first.Data.NextCursor), "", 400, 1, "INVALID_CURSOR", "cursor")
	request(caller, "kind=image&q="+url.QueryEscape("学习%_")+"&cursor="+url.QueryEscape(*first.Data.NextCursor), "", 400, 1, "INVALID_CURSOR", "cursor")
	request(caller, filters+"+&cursor="+url.QueryEscape(*first.Data.NextCursor), "", 400, 1, "INVALID_CURSOR", "cursor")
	directorySigner, err := service.NewDirectoryCursorSigner(configuration.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	directoryCursor, err := directorySigner.Sign(1, 12, domain.FileEntryPosition{ID: 15, ModifiedAt: time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	secrets = append(secrets, directoryCursor)
	request(caller, "cursor="+url.QueryEscape(directoryCursor), "", 400, 1, "INVALID_CURSOR", "cursor")
	globalSigner, err := service.NewGlobalEntryCursorSigner(configuration.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	globalCursor, err := globalSigner.Sign(1, domain.GlobalEntryQuery{Scope: "all", Sort: "modified_desc", PageSize: 50}, domain.FileEntryPosition{ID: 15, ModifiedAt: time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	recentSigner, err := service.NewRecentEntryCursorSigner(configuration.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	recentCursor, err := recentSigner.Sign(1, domain.GlobalEntryQuery{Scope: "recent", Sort: "last_opened_desc", PageSize: 50}, domain.RecentEntryPosition{ID: 15, LastOpenedAt: time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	nameSigner, err := service.NewNameEntryCursorSigner(configuration.SigningKey)
	if err != nil {
		t.Fatal(err)
	}
	nameCursor, err := nameSigner.Sign(1, domain.GlobalEntryQuery{Scope: "all", Sort: "name_asc", PageSize: 50}, domain.NameEntryPosition{ID: 15, NameKey: []byte("fixture.md")})
	if err != nil {
		t.Fatal(err)
	}
	for _, cursor := range []string{globalCursor, recentCursor, nameCursor} {
		secrets = append(secrets, cursor)
		request(caller, "cursor="+url.QueryEscape(cursor), "", 400, 1, "INVALID_CURSOR", "cursor")
	}

	// 7. 真实会话/设备/账号状态及读取失败按已有公开错误交付。
	for _, state := range []string{"revoked session", "expired session", "revoked device"} {
		token, actor := newLogin(1)
		switch state {
		case "revoked session":
			_, err = pool.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", actor.SessionID)
		case "expired session":
			_, err = pool.ExecContext(ctx, "UPDATE auth_sessions SET expires_at=UTC_TIMESTAMP(6)-INTERVAL 1 MINUTE WHERE id=?", actor.SessionID)
		case "revoked device":
			_, err = pool.ExecContext(ctx, "UPDATE user_devices SET revoked_at=UTC_TIMESTAMP(6) WHERE id=?", actor.DeviceID)
		}
		if err != nil {
			t.Fatal(err)
		}
		request(token, "limit=0", "", 401, 0, "SESSION_REVOKED", "")
	}
	request(caller, "", "snapshot-failure", 500, 1, "INTERNAL_ERROR", "")
	request(caller, "", "disable-after-auth", 403, 1, "ACCOUNT_DISABLED", "")
	request(caller, "limit=0", "", 403, 0, "ACCOUNT_DISABLED", "")
	if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET file_size=9007199254740992 WHERE id=15"); err != nil {
		t.Fatal(err)
	}
	request(caller, filters, "", 500, 1, "INTERNAL_ERROR", "")
	server.Close()
	for _, secret := range append(secrets, "private-parent", "private-query", "private-cookie", "private-invalid-cursor", "private-snapshot-failure", "学习%_") {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("真实大小完成日志包含凭证、查询或内部原文")
		}
	}
}
