package repository

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

type recentEntryListSnapshotReader struct {
	*Store
	calls     *int
	afterRead func() error
}

func (reader *recentEntryListSnapshotReader) ListUserRecentEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, before *domain.RecentEntryPosition) (*domain.RecentEntryPage, error) {
	(*reader.calls)++
	page, err := reader.Store.ListUserRecentEntryCandidates(ctx, userID, query, before)
	if err == nil && reader.afterRead != nil {
		err = reader.afterRead()
	}
	return page, err
}

// verifyRecentEntryList 复用正式候选夹具，装配真实快照和最近使用可见列表。
func verifyRecentEntryList(t *testing.T, ctx context.Context, pool *sql.DB, store *Store, stamp time.Time) {
	t.Helper()
	// 1. 把当前快照 Store 的候选及祖先读取交给正式服务。
	makeList := func(afterRead func() error) (*service.RecentEntryListService, *int) {
		calls := 0
		listing := service.NewRecentEntryListService(func(current context.Context, action func(context.Context, service.RecentEntryListReader) error) error {
			return store.WithFileEntrySnapshot(current, func(snapshotContext context.Context, reader *Store) error {
				return action(snapshotContext, &recentEntryListSnapshotReader{Store: reader, calls: &calls, afterRead: afterRead})
			})
		})
		return listing, &calls
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	masterKey := bytes.Repeat([]byte{11}, 32)
	cursorSigner, err := service.NewRecentEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	cursorVerifier, err := service.NewRecentEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	signPosition := func(t *testing.T, query domain.GlobalEntryQuery, position *domain.RecentEntryPosition) *domain.RecentEntryPosition {
		t.Helper()
		if position == nil {
			t.Fatal("真实最近列表缺少续页位置")
		}
		original := *position
		cursor, err := cursorSigner.Sign(actor.UserID, query, original)
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(cursor, ".")
		if len(parts) != 2 || len(cursor) > domain.MaxRecentEntryCursorLength {
			t.Fatal("真实最近签名游标形状错误")
		}
		payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
		if err != nil || base64.RawURLEncoding.EncodeToString(payloadBytes) != parts[0] {
			t.Fatal("真实最近游标载荷编码错误", err)
		}
		signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
		if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
			t.Fatal("真实最近游标签名编码错误", err)
		}
		derivation := hmac.New(sha256.New, masterKey)
		derivation.Write([]byte("cloudnest/recent-entry-cursor/v1"))
		messageMAC := hmac.New(sha256.New, derivation.Sum(nil))
		messageMAC.Write([]byte(parts[0]))
		if !hmac.Equal(signature, messageMAC.Sum(nil)) {
			t.Fatal("真实最近游标独立 HMAC 核对失败")
		}
		filters, err := json.Marshal(struct {
			Search string `json:"q"`
			Kind   string `json:"kind"`
		}{query.Search, query.Kind})
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(filters)
		var fields map[string]any
		if err := json.Unmarshal(payloadBytes, &fields); err != nil || len(fields) != 7 || fields["version"] != float64(1) || fields["user_id"] != strconv.FormatUint(actor.UserID, 10) || fields["scope"] != "recent" || fields["sort"] != "last_opened_desc" || fields["filter_hash"] != hex.EncodeToString(hash[:]) || fields["entry_id"] != strconv.FormatUint(original.ID, 10) || fields["last_opened_at"] != original.LastOpenedAt.UTC().Format("2006-01-02T15:04:05.000000Z") {
			t.Fatal("真实最近游标字段或访问位置错误", err)
		}
		openedAt, err := time.Parse("2006-01-02T15:04:05.000000Z", fields["last_opened_at"].(string))
		if err != nil {
			t.Fatal(err)
		}
		entryID, err := strconv.ParseUint(fields["entry_id"].(string), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		independent := domain.RecentEntryPosition{ID: entryID, LastOpenedAt: openedAt}
		resized := query
		resized.PageSize = 100
		restored, err := cursorVerifier.Verify(actor.UserID, resized, cursor)
		if err != nil || restored == nil || *restored != independent || restored == position || restored.LastOpenedAt.Location() != time.UTC {
			t.Fatal("正式最近使用校验器与独立位置不一致", err)
		}
		if again, err := cursorSigner.Sign(actor.UserID, resized, original); err != nil || again != cursor || *position != original || restored.ID != original.ID || !restored.LastOpenedAt.Equal(original.LastOpenedAt) || !restored.IsValid() {
			t.Fatal("真实最近游标页大小调整、位置还原或输入保护错误", err)
		}
		current, err := cursorVerifier.Verify(actor.UserID, query, cursor)
		if err != nil || current == nil || *current != *restored || current == restored {
			t.Fatal("当前页大小校验或结果副本错误", err)
		}
		// 在实际候选续读前，核对错误用户、筛选和文本均被拒绝。
		changedSearch, changedKind, changedScope := query, query, query
		changedSearch.Search += " "
		changedKind.Kind = "image"
		changedScope.Scope, changedScope.Sort = domain.GlobalEntryScopeAll, domain.GlobalEntrySortModifiedDesc
		for _, invalid := range []struct {
			userID uint64
			query  domain.GlobalEntryQuery
			cursor string
		}{
			{3, query, cursor},
			{actor.UserID, changedSearch, cursor},
			{actor.UserID, changedKind, cursor},
			{actor.UserID, changedScope, cursor},
			{actor.UserID, query, cursor + "A"},
		} {
			if result, err := cursorVerifier.Verify(invalid.userID, invalid.query, invalid.cursor); result != nil || !errors.Is(err, domain.ErrInvalidRecentEntryCursor) {
				t.Fatal("真实最近游标的错误绑定或文本通过校验", err)
			}
		}
		return restored
	}
	checkPage := func(t *testing.T, result *domain.RecentEntryList, ids []uint64, more bool, nextID uint64) {
		t.Helper()
		if result == nil || result.Entries == nil || len(result.Entries) != len(ids) || result.HasMore != more || (result.NextPosition != nil) != more {
			t.Fatal("真实最近可见列表形状或续页提示错误")
		}
		for index, candidate := range result.Entries {
			position := domain.RecentEntryPosition{ID: candidate.Entry.ID, LastOpenedAt: candidate.LastOpenedAt}
			if candidate.Entry.ID != ids[index] || candidate.Entry.EntryType != "FILE" || candidate.Entry.Status != "ACTIVE" || !position.IsValid() {
				t.Fatal("真实最近可见文件或访问时间错误")
			}
		}
		if more && (result.NextPosition.ID != nextID || !result.NextPosition.IsValid() || result.NextPosition.LastOpenedAt.Location() != time.UTC) {
			t.Fatal("真实最近列表最后扫描位置错误")
		}
	}
	makePage := func() (*service.RecentEntryPageService, *int) {
		listing, calls := makeList(nil)
		return service.NewRecentEntryPageService(cursorVerifier.Verify, listing.List, cursorSigner.Sign), calls
	}
	checkTextPage := func(t *testing.T, query domain.GlobalEntryQuery, result *domain.RecentEntryPageResult, ids []uint64, nextID uint64) {
		t.Helper()
		if result == nil || result.Entries == nil || len(result.Entries) != len(ids) || (result.NextCursor != nil) != (nextID != 0) {
			t.Fatal("真实最近文本分页形状或结束状态错误")
		}
		data, err := dto.NewRecentEntryPageData(result)
		if err != nil || data == nil || data.Items == nil || data.Breadcrumbs == nil || len(data.Items) != len(ids) || len(data.Breadcrumbs) != 0 || (data.NextCursor != nil) != (nextID != 0) {
			t.Fatal("真实最近分页公开数据或空数组错误", err)
		}
		if result.NextCursor != nil && (data.NextCursor == result.NextCursor || *data.NextCursor != *result.NextCursor) {
			t.Fatal("真实最近分页 DTO 游标副本错误")
		}
		const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
		encoded, err := json.Marshal(dto.RecentEntryPageResponse{Data: *data, RequestID: requestID})
		if err != nil || strings.Contains(string(encoded), `"user_id"`) {
			t.Fatal("真实最近分页公开 JSON 错误", err)
		}
		// 1. 将真实分页结果交给测试专用 GET 入口，复用正式交付与公共中间件。
		var logs bytes.Buffer
		router := serverhttp.NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
		router.GET("/test-recent-entry-page-success", func(requestContext *gin.Context) {
			handler.RecentEntryPageSuccess(requestContext, result)
		}, func(*gin.Context) { t.Error("真实最近分页交付后进入下游") })
		server := httptest.NewServer(router)
		defer server.Close()
		client := server.Client()
		client.Timeout = 2 * time.Second
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/test-recent-entry-page-success", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-Request-ID", requestID)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		server.Close()

		// 2. 核对真实传输头、完整 JSON 及请求完成日志，再逐字段对照数据库。
		if readError != nil || closeError != nil || response.StatusCode != 200 || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Content-Encoding") != "" || response.ContentLength != int64(len(body)) || len(response.Cookies()) != 0 || !bytes.Equal(body, encoded) {
			t.Fatal("真实最近分页 HTTP 状态、响应头或完整正文错误", readError, closeError)
		}
		var completed map[string]any
		if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["request_id"] != requestID || completed["status"] != float64(200) || completed["method"] != "GET" || completed["path"] != "/test-recent-entry-page-success" {
			t.Fatal("真实最近分页 HTTP 日志关联错误")
		}
		if strings.Contains(logs.String(), "学习笔记") || (result.NextCursor != nil && strings.Contains(logs.String(), *result.NextCursor)) {
			t.Fatal("真实最近分页完成日志包含业务名称或游标")
		}
		encoded = body
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &envelope); err != nil || len(envelope) != 2 || string(envelope["request_id"]) != `"`+requestID+`"` {
			t.Fatal("真实最近分页响应外层错误", err)
		}
		var publicData map[string]json.RawMessage
		if err := json.Unmarshal(envelope["data"], &publicData); err != nil || len(publicData) != 3 || string(publicData["breadcrumbs"]) != "[]" || (nextID == 0 && string(publicData["next_cursor"]) != "null") {
			t.Fatal("真实最近分页数据字段或空值错误", err)
		}
		var publicItems []map[string]json.RawMessage
		if err := json.Unmarshal(publicData["items"], &publicItems); err != nil || publicItems == nil || len(publicItems) != len(ids) {
			t.Fatal("真实最近分页 items 编码错误", err)
		}
		for index, candidate := range result.Entries {
			stored, err := store.FindUserFileEntry(ctx, actor.UserID, candidate.Entry.ID)
			if err != nil || stored == nil || candidate.Entry.ID != ids[index] || !reflect.DeepEqual(candidate.Entry, *stored) {
				t.Fatal("最近文本分页丢失文件基础字段", err)
			}
			preference, err := store.FindUserEntryPreference(ctx, actor.UserID, candidate.Entry.ID)
			if err != nil || preference == nil || preference.LastOpenedAt == nil || !candidate.LastOpenedAt.Equal(*preference.LastOpenedAt) {
				t.Fatal("最近文本分页丢失访问事实", err)
			}
			item := data.Items[index]
			if len(publicItems[index]) != 11 || item.ID != strconv.FormatUint(candidate.Entry.ID, 10) || item.ParentID != strconv.FormatUint(*candidate.Entry.ParentID, 10) || item.Revision != strconv.FormatUint(candidate.Entry.Revision, 10) || item.Name != candidate.Entry.Name || item.SizeBytes != candidate.Entry.FileSize || item.EntryType != "FILE" || item.Status != "ACTIVE" || !item.CreatedAt.Equal(candidate.Entry.CreatedAt) || !item.ModifiedAt.Equal(candidate.Entry.ModifiedAt) || !item.UpdatedAt.Equal(candidate.Entry.UpdatedAt) || !item.LastOpenedAt.Equal(candidate.LastOpenedAt) || item.LastOpenedAt.Location() != time.UTC || string(publicItems[index]["last_opened_at"]) != `"`+item.LastOpenedAt.Format(time.RFC3339Nano)+`"` {
				t.Fatal("真实最近文件十一字段与数据库事实不一致")
			}
		}
		if nextID == 0 {
			return
		}
		position, err := cursorVerifier.Verify(actor.UserID, query, *result.NextCursor)
		if err != nil || position == nil || position.ID != nextID {
			t.Fatal("最近文本分页下一页签名位置错误", err)
		}
		preference, err := store.FindUserEntryPreference(ctx, actor.UserID, nextID)
		if err != nil || preference == nil || preference.LastOpenedAt == nil || !position.LastOpenedAt.Equal(*preference.LastOpenedAt) {
			t.Fatal("最近文本游标访问时间与最后扫描项不符", err)
		}
	}
	t.Run("text page first next end and binding", func(t *testing.T) {
		paging, calls := makePage()
		query := recentEntryCandidateQuery(1)
		first, err := paging.List(ctx, actor, query, "")
		if err != nil {
			t.Fatal(err)
		}
		checkTextPage(t, query, first, []uint64{103}, 103)
		// 有效页大小变化可复用原文本，第二页在隐藏过滤后填满两项。
		resized := query
		resized.PageSize = 2
		resizedPage, err := paging.List(ctx, actor, resized, *first.NextCursor)
		if err != nil {
			t.Fatal(err)
		}
		checkTextPage(t, resized, resizedPage, []uint64{101, 105}, 0)
		second, err := paging.List(ctx, actor, query, *first.NextCursor)
		if err != nil {
			t.Fatal(err)
		}
		checkTextPage(t, query, second, []uint64{101}, 101)
		third, err := paging.List(ctx, actor, query, *second.NextCursor)
		if err != nil {
			t.Fatal(err)
		}
		checkTextPage(t, query, third, []uint64{105}, 0)
		emptyQuery := query
		emptyQuery.Search = "没有匹配的最近文件"
		empty, err := paging.List(ctx, actor, emptyQuery, "")
		if err != nil {
			t.Fatal(err)
		}
		checkTextPage(t, emptyQuery, empty, nil, 0)

		// 错误用户、筛选或文本在读取候选前返回。
		changedSearch, changedKind := query, query
		changedSearch.Search = "学习"
		changedKind.Kind = "document"
		beforeCalls := *calls
		for _, invalid := range []struct {
			userID uint64
			query  domain.GlobalEntryQuery
			cursor string
		}{
			{3, query, *first.NextCursor},
			{actor.UserID, changedSearch, *first.NextCursor},
			{actor.UserID, changedKind, *first.NextCursor},
			{actor.UserID, query, *first.NextCursor + "A"},
		} {
			identity := actor
			identity.UserID = invalid.userID
			if result, err := paging.List(ctx, identity, invalid.query, invalid.cursor); result != nil || !errors.Is(err, domain.ErrInvalidRecentEntryCursor) || *calls != beforeCalls {
				t.Fatal("无效最近文本游标进入候选读取", err)
			}
		}
	})
	t.Run("visible pages and access continuation", func(t *testing.T) {
		listing, _ := makeList(nil)
		query := recentEntryCandidateQuery(1)
		var before *domain.RecentEntryPosition
		for index, ids := range [][]uint64{{103}, {101}, {105}, {}} {
			result, err := listing.List(ctx, actor, query, before)
			if err != nil {
				t.Fatal(err)
			}
			var nextID uint64
			if index < 2 {
				nextID = ids[0]
			}
			checkPage(t, result, ids, index < 2, nextID)
			if len(result.Entries) > 0 {
				last := result.Entries[0]
				before = &domain.RecentEntryPosition{ID: last.Entry.ID, LastOpenedAt: last.LastOpenedAt.In(time.FixedZone("fixture", 8*3600))}
				if result.NextPosition != nil {
					before = signPosition(t, query, result.NextPosition)
					before.LastOpenedAt = before.LastOpenedAt.In(time.FixedZone("fixture", 8*3600))
				}
				if last.Entry.ID == 103 && !last.LastOpenedAt.Equal(stamp.Add(2*time.Second)) {
					t.Fatal("最近列表丢失原访问时间")
				}
			}
		}
	})
	t.Run("search kind ownership and account status", func(t *testing.T) {
		listing, _ := makeList(nil)
		for _, fixture := range []struct {
			search, kind string
			ids          []uint64
		}{
			{"访问", "document", []uint64{103}},
			{"访问", "image", []uint64{105}},
			{"学习", "document", []uint64{101}},
			{"隐藏", "document", nil},
			{"", "folder", nil},
		} {
			query := recentEntryCandidateQuery(50)
			query.Search, query.Kind = fixture.search, fixture.kind
			result, err := listing.List(ctx, actor, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkPage(t, result, fixture.ids, false, 0)
		}
		other := actor
		other.UserID = 3
		foreign, err := listing.List(ctx, other, recentEntryCandidateQuery(2), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, foreign, []uint64{121}, false, 0)
		other.UserID = 999
		if result, err := listing.List(ctx, other, recentEntryCandidateQuery(2), nil); result != nil || !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatal("空候选跳过用户状态检查", err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=5"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE users SET status='ACTIVE' WHERE id=5"); err != nil {
				t.Error("恢复账号状态失败", err)
			}
		}()
		other.UserID = 5
		if result, err := listing.List(ctx, other, recentEntryCandidateQuery(2), nil); result != nil || !errors.Is(err, domain.ErrAccountDisabled) {
			t.Fatal("空最近列表跳过禁用账号检查", err)
		}
	})
	t.Run("single connection", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		listing, _ := makeList(nil)
		result, err := listing.List(ctx, actor, recentEntryCandidateQuery(10), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, result, []uint64{103, 101, 105}, false, 0)
	})
	// 2. 在候选读取后提交外部变化，确认访问与祖先检查使用原快照。
	t.Run("parent move after candidate read", func(t *testing.T) {
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=14 WHERE id=103"); err != nil {
				t.Error("恢复最近文件父目录失败", err)
			}
		}()
		listing, _ := makeList(func() error {
			_, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=16 WHERE id=103")
			return err
		})
		result, err := listing.List(ctx, actor, recentEntryCandidateQuery(10), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, result, []uint64{103, 101, 105}, false, 0)
		current, _ := makeList(nil)
		again, err := current.List(ctx, actor, recentEntryCandidateQuery(10), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, again, []uint64{101, 105}, false, 0)
	})
	t.Run("access changes after candidate read", func(t *testing.T) {
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=IF(entry_id=103,?,NULL) WHERE user_id=1 AND entry_id IN (103,104)", stamp.Add(2*time.Second)); err != nil {
				t.Error("恢复最近文件访问偏好失败", err)
			}
		}()
		listing, _ := makeList(func() error {
			_, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=IF(entry_id=103,NULL,?) WHERE user_id=1 AND entry_id IN (103,104)", stamp.Add(5*time.Second))
			return err
		})
		result, err := listing.List(ctx, actor, recentEntryCandidateQuery(10), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, result, []uint64{103, 101, 105}, false, 0)
		current, _ := makeList(nil)
		again, err := current.List(ctx, actor, recentEntryCandidateQuery(10), nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, again, []uint64{104, 101, 105}, false, 0)
	})

	// 3. 在独占库准备一千零一个隐藏候选及更早的两个可见文件。
	defer func() {
		for _, statement := range []string{
			"DELETE FROM user_entry_preferences WHERE entry_id IN (2901,2903) OR entry_id BETWEEN 3000 AND 4000",
			"DELETE FROM file_entries WHERE id IN (2901,2903) OR id BETWEEN 3000 AND 4000",
		} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Error("清理最近列表预算夹具失败", err)
			}
		}
	}()
	writer, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	for index := 0; index < 1003; index++ {
		entryID, parentID := 3000+index, 16
		name := fmt.Sprintf("最近预算-隐藏-%d.md", index)
		openedAt := stamp.Add(3 * time.Second)
		if index >= 1001 {
			entryID, parentID = 2901+(index-1001)*2, 12
			name = fmt.Sprintf("最近预算-可见-%d.md", entryID)
			openedAt = stamp.Add(-time.Second)
		}
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,?,?,?,'FILE','DOCUMENT',41,2048)", entryID, parentID, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,last_opened_at) VALUES(1,?,?)", entryID, openedAt); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	budgetQuery := recentEntryCandidateQuery(2)
	budgetQuery.Search, budgetQuery.Kind = "最近预算-", "document"
	t.Run("empty budget continues to visible files", func(t *testing.T) {
		listing, calls := makeList(nil)
		first, err := listing.List(ctx, actor, budgetQuery, nil)
		if err != nil || *calls != 10 {
			t.Fatal("最近预算扫描批次错误", err)
		}
		checkPage(t, first, nil, true, 3001)
		if !first.NextPosition.LastOpenedAt.Equal(stamp.Add(3 * time.Second)) {
			t.Fatal("隐藏末项访问时间丢失")
		}
		next, err := listing.List(ctx, actor, budgetQuery, signPosition(t, budgetQuery, first.NextPosition))
		if err != nil || *calls != 11 {
			t.Fatal("最近预算空页续读错误", err)
		}
		checkPage(t, next, []uint64{2903, 2901}, false, 0)
	})
	t.Run("text budget empty next and end", func(t *testing.T) {
		paging, calls := makePage()
		first, err := paging.List(ctx, actor, budgetQuery, "")
		if err != nil || *calls != 10 {
			t.Fatal("文本预算空页读取批次错误", err)
		}
		checkTextPage(t, budgetQuery, first, nil, 3001)
		next, err := paging.List(ctx, actor, budgetQuery, *first.NextCursor)
		if err != nil || *calls != 11 {
			t.Fatal("文本预算游标续读错误", err)
		}
		checkTextPage(t, budgetQuery, next, []uint64{2903, 2901}, 0)
	})
	t.Run("short page uses hidden last position", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=12 WHERE id=4000"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=16 WHERE id=4000"); err != nil {
				t.Error(err)
			}
		}()
		listing, _ := makeList(nil)
		first, err := listing.List(ctx, actor, budgetQuery, nil)
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, first, []uint64{4000}, true, 3001)
		next, err := listing.List(ctx, actor, budgetQuery, signPosition(t, budgetQuery, first.NextPosition))
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, next, []uint64{2903, 2901}, false, 0)
		paging, _ := makePage()
		textFirst, err := paging.List(ctx, actor, budgetQuery, "")
		if err != nil {
			t.Fatal(err)
		}
		checkTextPage(t, budgetQuery, textFirst, []uint64{4000}, 3001)
		textNext, err := paging.List(ctx, actor, budgetQuery, *textFirst.NextCursor)
		if err != nil {
			t.Fatal(err)
		}
		checkTextPage(t, budgetQuery, textNext, []uint64{2903, 2901}, 0)
	})
	t.Run("full page at budget edge", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=12 WHERE id=3001"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=16 WHERE id=3001"); err != nil {
				t.Error(err)
			}
		}()
		listing, calls := makeList(nil)
		query := budgetQuery
		query.PageSize = 1
		first, err := listing.List(ctx, actor, query, nil)
		if err != nil || *calls != 10 {
			t.Fatal("预算边界满页错误", err)
		}
		checkPage(t, first, []uint64{3001}, true, 3001)
		next, err := listing.List(ctx, actor, query, signPosition(t, query, first.NextPosition))
		if err != nil {
			t.Fatal(err)
		}
		checkPage(t, next, []uint64{2903}, true, 2903)
	})
	t.Run("exact budget end", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=3000"); err != nil {
			t.Fatal(err)
		}
		listing, calls := makeList(nil)
		query := budgetQuery
		query.Search = "最近预算-隐藏-"
		result, err := listing.List(ctx, actor, query, nil)
		if err != nil || *calls != 10 {
			t.Fatal("恰好最近预算结束错误", err)
		}
		checkPage(t, result, nil, false, 0)
		paging, textCalls := makePage()
		textEnd, err := paging.List(ctx, actor, query, "")
		if err != nil || *textCalls != 10 {
			t.Fatal("文本分页恰好预算结束错误", err)
		}
		checkTextPage(t, query, textEnd, nil, 0)
	})
}
