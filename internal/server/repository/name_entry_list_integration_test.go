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
	"net/url"
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

type nameEntryListSnapshotReader struct {
	*Store
	calls     *int
	afterRead func() error
}

func (reader *nameEntryListSnapshotReader) ListUserNameEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, after *domain.NameEntryPosition) (*domain.NameEntryPage, error) {
	(*reader.calls)++
	page, err := reader.Store.ListUserNameEntryCandidates(ctx, userID, query, after)
	if err == nil && reader.afterRead != nil {
		err = reader.afterRead()
	}
	return page, err
}

func verifyNameEntryList(t *testing.T, ctx context.Context, pool *sql.DB, store *Store) {
	t.Helper()
	// 1. 绑定同一快照中的名称候选与祖先读取，检查每项数据库事实。
	makeList := func(afterRead func() error) (*service.NameEntryListService, *int) {
		calls := 0
		listing := service.NewNameEntryListService(func(current context.Context, action func(context.Context, service.NameEntryListReader) error) error {
			return store.WithFileEntrySnapshot(current, func(snapshotContext context.Context, reader *Store) error {
				return action(snapshotContext, &nameEntryListSnapshotReader{Store: reader, calls: &calls, afterRead: afterRead})
			})
		})
		return listing, &calls
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	masterKey := bytes.Repeat([]byte{11}, 32)
	signer, err := service.NewNameEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewNameEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	signPosition := func(t *testing.T, query domain.GlobalEntryQuery, position *domain.NameEntryPosition) *domain.NameEntryPosition {
		t.Helper()
		if position == nil {
			return nil
		}
		// 1. 使用正式签名器，再独立检查文本编码和名称用途 HMAC。
		original := domain.NameEntryPosition{NameKey: bytes.Clone(position.NameKey), ID: position.ID}
		cursor, err := signer.Sign(actor.UserID, query, *position)
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(cursor, ".")
		if len(parts) != 2 || len(cursor) > domain.MaxNameEntryCursorLength || strings.ContainsAny(cursor, "=+/ \r\n") {
			t.Fatal("真实名称签名游标形状错误")
		}
		payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
		if err != nil || base64.RawURLEncoding.EncodeToString(payloadBytes) != parts[0] {
			t.Fatal("真实名称载荷编码错误", err)
		}
		signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
		if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
			t.Fatal("真实名称签名编码错误", err)
		}
		derivation := hmac.New(sha256.New, masterKey)
		derivation.Write([]byte("cloudnest/name-entry-cursor/v1"))
		messageMAC := hmac.New(sha256.New, derivation.Sum(nil))
		messageMAC.Write([]byte(parts[0]))
		if !hmac.Equal(signature, messageMAC.Sum(nil)) {
			t.Fatal("真实名称游标独立 HMAC 不符")
		}

		// 2. 独立核对七字段、筛选摘要及可逆名称键，并从文本还原位置。
		var fields map[string]json.RawMessage
		var payload struct {
			Version    int    `json:"version"`
			UserID     string `json:"user_id"`
			Scope      string `json:"scope"`
			Sort       string `json:"sort"`
			FilterHash string `json:"filter_hash"`
			NameKey    string `json:"name_key"`
			EntryID    string `json:"entry_id"`
		}
		filters, err := json.Marshal(struct {
			Search string `json:"q"`
			Kind   string `json:"kind"`
		}{query.Search, query.Kind})
		if err != nil {
			t.Fatal(err)
		}
		filterHash := sha256.Sum256(filters)
		if json.Unmarshal(payloadBytes, &fields) != nil || len(fields) != 7 || json.Unmarshal(payloadBytes, &payload) != nil || payload.Version != 1 || payload.UserID != strconv.FormatUint(actor.UserID, 10) || payload.Scope != query.Scope || payload.Sort != "name_asc" || payload.FilterHash != hex.EncodeToString(filterHash[:]) {
			t.Fatal("真实名称载荷字段或筛选摘要错误")
		}
		nameKey, err := base64.RawURLEncoding.Strict().DecodeString(payload.NameKey)
		if err != nil || base64.RawURLEncoding.EncodeToString(nameKey) != payload.NameKey {
			t.Fatal("真实名称键编码错误", err)
		}
		entryID, err := strconv.ParseUint(payload.EntryID, 10, 64)
		if err != nil || payload.EntryID != strconv.FormatUint(original.ID, 10) || !bytes.Equal(nameKey, original.NameKey) || !reflect.DeepEqual(*position, original) {
			t.Fatal("真实名称位置或输入保护错误", err)
		}
		resized := query
		resized.PageSize = 100
		if repeated, err := signer.Sign(actor.UserID, resized, original); err != nil || repeated != cursor {
			t.Fatal("真实名称游标页大小调整改变绑定", err)
		}
		// 3. 正式校验器与独立位置对照，错误用户、筛选和文本在续读前拒绝。
		independent := domain.NameEntryPosition{NameKey: nameKey, ID: entryID}
		restored, err := verifier.Verify(actor.UserID, resized, cursor)
		if err != nil || restored == nil || !reflect.DeepEqual(*restored, independent) || restored == position || !reflect.DeepEqual(*position, original) {
			t.Fatal("正式名称校验器未还原独立位置", err)
		}
		current, err := verifier.Verify(actor.UserID, query, cursor)
		if err != nil || current == nil || current == restored || !reflect.DeepEqual(*current, *restored) {
			t.Fatal("名称校验结果共享位置", err)
		}
		restored.NameKey[0] ^= 1
		if !bytes.Equal(current.NameKey, independent.NameKey) {
			t.Fatal("名称还原结果共享字节")
		}
		changedSearch, changedKind, changedScope := query, query, query
		changedSearch.Search += " "
		changedKind.Kind = "image"
		if query.Scope == "all" {
			changedScope.Scope = "starred"
		} else {
			changedScope.Scope = "all"
		}
		for _, invalid := range []struct {
			userID uint64
			query  domain.GlobalEntryQuery
			cursor string
		}{
			{3, query, cursor}, {actor.UserID, changedSearch, cursor}, {actor.UserID, changedKind, cursor}, {actor.UserID, changedScope, cursor}, {actor.UserID, query, cursor + "A"},
		} {
			if result, err := verifier.Verify(invalid.userID, invalid.query, invalid.cursor); result != nil || !errors.Is(err, domain.ErrInvalidNameEntryCursor) {
				t.Fatal("名称错误游标绑定通过校验", err)
			}
		}
		return current
	}
	restore := func(statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Error("恢复名称列表夹具失败", err)
		}
	}
	check := func(t *testing.T, result *domain.NameEntryList, ids []uint64, nextID uint64) {
		t.Helper()
		if result == nil || result.Entries == nil || len(result.Entries) != len(ids) || result.HasMore != (nextID != 0) || (result.NextPosition != nil) != (nextID != 0) {
			t.Fatal("名称可见列表形状或位置错误")
		}
		for index, candidate := range result.Entries {
			stored, err := store.FindUserFileEntry(ctx, candidate.Entry.UserID, candidate.Entry.ID)
			var key []byte
			keyError := pool.QueryRowContext(ctx, "SELECT name_key FROM file_entries WHERE user_id=? AND id=?", candidate.Entry.UserID, candidate.Entry.ID).Scan(&key)
			if err != nil || keyError != nil || stored == nil || candidate.Entry.ID != ids[index] || !reflect.DeepEqual(candidate.Entry, *stored) || !bytes.Equal(candidate.NameKey, key) {
				t.Fatal("名称可见项或名称键与数据库不符", err, keyError)
			}
		}
		if nextID != 0 {
			var key []byte
			if err := pool.QueryRowContext(ctx, "SELECT name_key FROM file_entries WHERE user_id=1 AND id=?", nextID).Scan(&key); err != nil || result.NextPosition.ID != nextID || !result.NextPosition.IsValid() || !bytes.Equal(result.NextPosition.NameKey, key) {
				t.Fatal("最后消费名称位置与数据库不符", err)
			}
		}
	}
	checkTextPage := func(t *testing.T, listing *service.NameEntryListService, query domain.GlobalEntryQuery, cursor string, ids []uint64, nextID uint64) string {
		t.Helper()
		// 1. 解析 URL 原文后组合正式能力，检查类型化条件及可信身份。
		values := url.Values{
			"scope": {query.Scope}, "sort": {query.Sort}, "q": {query.Search}, "kind": {query.Kind},
			"limit": {strconv.Itoa(query.PageSize)}, "cursor": {cursor}, "user_id": {"99"}, "parent_id": {"private-parent"},
		}
		input, fieldError := dto.ParseNameEntryPageQuery(values.Encode())
		if fieldError != nil || input == nil || input.Query != query || input.Cursor != cursor {
			t.Fatal("真实名称分页 URL 条件或原文错误", fieldError)
		}
		listCalls := 0
		paging := service.NewNameEntryPageService(verifier.Verify,
			func(current context.Context, identity domain.Actor, filters domain.GlobalEntryQuery, after *domain.NameEntryPosition) (*domain.NameEntryList, error) {
				listCalls++
				return listing.List(current, identity, filters, after)
			}, signer.Sign)
		result, err := paging.List(ctx, actor, input.Query, input.Cursor)
		if err != nil || result == nil || result.Entries == nil || (result.NextCursor != nil) != (nextID != 0) || listCalls != 1 {
			t.Fatal("名称文本分页结果错误", err)
		}
		// 2. 转换真实分页结果，独立核对公开字段、空值和来源副本。
		data, err := dto.NewNameEntryPageData(result)
		if err != nil || data == nil || data.Items == nil || data.Breadcrumbs == nil || len(data.Items) != len(result.Entries) || len(data.Breadcrumbs) != 0 || (data.NextCursor != nil) != (result.NextCursor != nil) {
			t.Fatal("真实名称分页响应转换失败", err)
		}
		const requestID = "28ee758a-0727-42de-ae7d-3bb33155e148"
		encoded, err := json.Marshal(dto.NameEntryPageResponse{Data: *data, RequestID: requestID})
		if err != nil {
			t.Fatal(err)
		}
		// 3. 通过系统分配端口交付真实查询结果，核对传输和请求日志。
		var logs bytes.Buffer
		router := serverhttp.NewRouter(logging.New(&logs), nil, func(context.Context) bool { return true })
		router.GET("/test-name-entry-page-success", func(requestContext *gin.Context) {
			parsed, fieldError := dto.ParseNameEntryPageQuery(requestContext.Request.URL.RawQuery)
			if fieldError != nil {
				handler.InvalidArgument(requestContext, fieldError)
				return
			}
			if parsed.Query != input.Query || parsed.Cursor != input.Cursor {
				t.Error("实际 GET 未保留名称查询条件")
			}
			handler.NameEntryPageSuccess(requestContext, result)
		}, func(*gin.Context) { t.Error("真实名称分页交付后进入下游") })
		server := httptest.NewServer(router)
		defer server.Close()
		client := server.Client()
		client.Timeout = 2 * time.Second
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/test-name-entry-page-success?"+values.Encode(), nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-Request-ID", strings.ToUpper(requestID))
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		server.Close()
		if readError != nil || closeError != nil || response.StatusCode != 200 || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Content-Encoding") != "" || response.ContentLength != int64(len(body)) || len(response.Cookies()) != 0 || !bytes.Equal(body, encoded) {
			t.Fatal("真实名称分页 HTTP 状态、响应头或完整正文错误", readError, closeError)
		}
		var completed map[string]any
		if json.Unmarshal(logs.Bytes(), &completed) != nil || completed["request_id"] != requestID || completed["status"] != float64(200) || completed["method"] != "GET" || completed["path"] != "/test-name-entry-page-success" {
			t.Fatal("真实名称分页 HTTP 日志关联错误")
		}
		if result.NextCursor != nil && strings.Contains(logs.String(), *result.NextCursor) {
			t.Fatal("真实名称分页完成日志包含游标")
		}
		encoded = body
		var envelope map[string]json.RawMessage
		var payload map[string]json.RawMessage
		if json.Unmarshal(encoded, &envelope) != nil || len(envelope) != 2 || string(envelope["request_id"]) != strconv.Quote(requestID) || json.Unmarshal(envelope["data"], &payload) != nil || len(payload) != 3 || string(payload["breadcrumbs"]) != "[]" {
			t.Fatal("真实名称分页响应外层或导航错误")
		}
		var items []map[string]json.RawMessage
		if json.Unmarshal(payload["items"], &items) != nil || items == nil || len(items) != len(result.Entries) {
			t.Fatal("真实名称分页响应条目数组错误")
		}
		for index, item := range items {
			entry := result.Entries[index].Entry
			expected := map[string]any{
				"id": strconv.FormatUint(entry.ID, 10), "parent_id": strconv.FormatUint(*entry.ParentID, 10),
				"name": entry.Name, "entry_type": entry.EntryType, "size_bytes": entry.FileSize,
				"revision": strconv.FormatUint(entry.Revision, 10), "status": entry.Status,
				"created_at": entry.CreatedAt.UTC().Truncate(time.Microsecond), "modified_at": entry.ModifiedAt.UTC().Truncate(time.Microsecond), "updated_at": entry.UpdatedAt.UTC().Truncate(time.Microsecond),
			}
			if len(item) != len(expected) {
				t.Fatal("真实名称分页条目字段数量错误")
			}
			for field, value := range expected {
				fieldJSON, err := json.Marshal(value)
				if err != nil || !bytes.Equal(item[field], fieldJSON) {
					t.Fatal("真实名称分页字段与已确认记录不符", field, err)
				}
			}
		}
		cursorJSON := "null"
		if result.NextCursor != nil {
			cursorJSON = strconv.Quote(*result.NextCursor)
			if data.NextCursor == result.NextCursor || *data.NextCursor != *result.NextCursor {
				t.Fatal("真实名称分页 DTO 游标副本错误")
			}
		}
		if string(payload["next_cursor"]) != cursorJSON {
			t.Fatal("真实名称分页 DTO 游标文本或 null 错误")
		}
		if data.NextCursor != nil {
			*data.NextCursor = "local-name-cursor"
			if strconv.Quote(*result.NextCursor) != cursorJSON {
				t.Fatal("真实名称分页 DTO 游标修改影响来源")
			}
		}
		// 4. 与数据库事实和原独立协议检查对照，再把实际文本交给下页。
		listed := &domain.NameEntryList{Entries: result.Entries, HasMore: result.NextCursor != nil}
		if result.NextCursor == nil {
			check(t, listed, ids, 0)
			return ""
		}
		listed.NextPosition, err = verifier.Verify(actor.UserID, query, *result.NextCursor)
		if err != nil {
			t.Fatal("名称分页返回无法校验的文本", err)
		}
		check(t, listed, ids, nextID)
		independent := signPosition(t, query, listed.NextPosition)
		repeated, err := signer.Sign(actor.UserID, query, *independent)
		if err != nil || repeated != *result.NextCursor {
			t.Fatal("名称文本分页与独立协议对照不符", err)
		}
		resized := query
		resized.PageSize = 100
		if position, err := verifier.Verify(actor.UserID, resized, *result.NextCursor); err != nil || !reflect.DeepEqual(position, independent) {
			t.Fatal("名称文本分页不能调整合法页大小", err)
		}
		changed := query
		changed.Search += " "
		if failed, err := paging.List(ctx, actor, changed, *result.NextCursor); failed != nil || !errors.Is(err, domain.ErrInvalidNameEntryCursor) || listCalls != 1 {
			t.Fatal("名称文本分页错误筛选进入列表", err)
		}
		if failed, err := paging.List(ctx, actor, query, *result.NextCursor+"A"); failed != nil || !errors.Is(err, domain.ErrInvalidNameEntryCursor) || listCalls != 1 {
			t.Fatal("名称文本分页篡改游标进入列表", err)
		}
		return *result.NextCursor
	}
	t.Run("all and starred visible pagination", func(t *testing.T) {
		for _, scope := range []string{"all", "starred"} {
			listing, _ := makeList(nil)
			query := nameEntryCandidateQuery(2)
			query.Scope = scope
			expected := [][]uint64{{105, 107}, {12, 14}, {101, 103}, {109}}
			if scope == "starred" {
				expected = [][]uint64{{107, 101}, {103}}
			}
			var after *domain.NameEntryPosition
			cursor := ""
			for index, ids := range expected {
				result, err := listing.List(ctx, actor, query, after)
				if err != nil {
					t.Fatal(err)
				}
				var nextID uint64
				if index+1 < len(expected) {
					nextID = ids[len(ids)-1]
				}
				check(t, result, ids, nextID)
				after = signPosition(t, query, result.NextPosition)
				cursor = checkTextPage(t, listing, query, cursor, ids, nextID)
			}
			if cursor != "" {
				t.Fatal("名称文本结束页仍有游标")
			}
		}
	})
	t.Run("filters users and account state", func(t *testing.T) {
		listing, _ := makeList(nil)
		for _, fixture := range []struct {
			scope, kind, search string
			userID              uint64
			ids                 []uint64
		}{
			{scope: "all", userID: 1, kind: "folder", ids: []uint64{107, 12, 14}},
			{scope: "all", userID: 1, kind: "document", search: "readme", ids: []uint64{101, 103}},
			{scope: "starred", userID: 1, kind: "document", ids: []uint64{101, 103}},
			{scope: "all", userID: 1, search: "b-hidden"},
			{scope: "all", userID: 3, ids: []uint64{121}},
			{scope: "all", userID: 5},
		} {
			query := nameEntryCandidateQuery(100)
			query.Scope, query.Kind, query.Search = fixture.scope, fixture.kind, fixture.search
			identity := actor
			identity.UserID = fixture.userID
			result, err := listing.List(ctx, identity, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			check(t, result, fixture.ids, 0)
			paging := service.NewNameEntryPageService(verifier.Verify, listing.List, signer.Sign)
			page, err := paging.List(ctx, identity, query, "")
			if err != nil || page == nil || page.NextCursor != nil || !reflect.DeepEqual(page.Entries, result.Entries) {
				t.Fatal("名称文本分页用户或筛选事实错误", err)
			}
		}
		identity := actor
		identity.UserID = 999
		if result, err := listing.List(ctx, identity, nameEntryCandidateQuery(2), nil); result != nil || !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatal("空名称候选跳过账号检查", err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=5"); err != nil {
			t.Fatal(err)
		}
		defer restore("UPDATE users SET status='ACTIVE' WHERE id=5")
		identity.UserID = 5
		if result, err := listing.List(ctx, identity, nameEntryCandidateQuery(2), nil); result != nil || !errors.Is(err, domain.ErrAccountDisabled) {
			t.Fatal("空候选禁用账号被放行", err)
		}
	})
	t.Run("single connection", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		listing, _ := makeList(nil)
		result, err := listing.List(ctx, actor, nameEntryCandidateQuery(100), nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, result, []uint64{105, 107, 12, 14, 101, 103, 109}, 0)
		_ = checkTextPage(t, listing, nameEntryCandidateQuery(100), "", []uint64{105, 107, 12, 14, 101, 103, 109}, 0)
	})
	t.Run("parent move snapshot", func(t *testing.T) {
		defer restore("UPDATE file_entries SET parent_id=11 WHERE id=14")
		changed := false
		listing, _ := makeList(func() error {
			if changed {
				return nil
			}
			changed = true
			_, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=16 WHERE id=14")
			return err
		})
		query := nameEntryCandidateQuery(100)
		query.Scope = "starred"
		result, err := listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result == nil || len(result.Entries) != 3 || result.Entries[2].Entry.ID != 103 {
			t.Fatal("原快照丢失移动前可见项")
		}
		current, _ := makeList(nil)
		result, err = current.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, result, []uint64{107, 101}, 0)
	})
	t.Run("name and membership snapshots", func(t *testing.T) {
		defer restore("UPDATE file_entries SET name='readme.md',name_key=CAST('readme.md' AS BINARY) WHERE id=103")
		defer restore("UPDATE user_entry_preferences SET starred_at=UTC_TIMESTAMP(6) WHERE user_id=1 AND entry_id=101")
		changed := false
		listing, _ := makeList(func() error {
			if changed {
				return nil
			}
			changed = true
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET name='AA.md',name_key=CAST('aa.md' AS BINARY) WHERE id=103"); err != nil {
				return err
			}
			_, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=NULL WHERE user_id=1 AND entry_id=101")
			return err
		})
		query := nameEntryCandidateQuery(100)
		query.Scope = "starred"
		original, err := listing.List(ctx, actor, query, nil)
		if err != nil || original == nil || len(original.Entries) != 3 || original.Entries[1].Entry.ID != 101 || original.Entries[2].Entry.ID != 103 || string(original.Entries[2].NameKey) != "readme.md" || original.Entries[2].Entry.Name != "readme.md" {
			t.Fatal("快照未保留原名称、键或收藏成员", err)
		}
		current, _ := makeList(nil)
		result, err := current.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, result, []uint64{103, 107}, 0)
	})

	t.Run("long binary text cursor and resized page", func(t *testing.T) {
		// 1. 保存两个同键项，令实际文本游标超过一千零二十四字节。
		defer restore("DELETE FROM file_entries WHERE user_id=1 AND id IN (501,502)")
		defer restore("DELETE FROM user_entry_preferences WHERE user_id=1 AND entry_id IN (501,502)")
		key := bytes.Repeat([]byte{0xff}, 768)
		for _, entry := range []struct {
			entryID  uint64
			parentID uint64
		}{{501, 12}, {502, 14}} {
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,?,?,?,'FILE','DOCUMENT',41,2048)", entry.entryID, entry.parentID, fmt.Sprintf("name-text-long-%d", entry.entryID), key); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) VALUES(1,?,UTC_TIMESTAMP(6))", entry.entryID); err != nil {
				t.Fatal(err)
			}
		}
		for _, scope := range []string{"all", "starred"} {
			listing, _ := makeList(nil)
			query := nameEntryCandidateQuery(1)
			query.Scope, query.Search = scope, "name-text-long-"
			cursor := checkTextPage(t, listing, query, "", []uint64{501}, 501)
			if len(cursor) <= 1024 || len(cursor) > domain.MaxNameEntryCursorLength {
				t.Fatal("名称最长键未经过正确文本容量边界")
			}
			// 2. 使用原文本调整页大小，继续同键下一个 ID。
			query.PageSize = 100
			_ = checkTextPage(t, listing, query, cursor, []uint64{502}, 0)
		}
	})

	// 2. 准备一千零一项隐藏候选，验证有界补读及最后消费位置。
	writer, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	for index := 0; index < 1001; index++ {
		name := fmt.Sprintf("name-budget-hidden-%04d", index)
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,16,?,?,'FILE','DOCUMENT',41,2048)", 3000+index, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct {
		id   uint64
		name string
	}{{2901, "name-budget-visible-a"}, {2903, "name-budget-visible-b"}} {
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,12,?,?,'FILE','DOCUMENT',41,2048)", entry.id, entry.name, []byte(entry.name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) SELECT user_id,id,UTC_TIMESTAMP(6) FROM file_entries WHERE user_id=1 AND (id IN (2901,2903) OR id BETWEEN 3000 AND 4000)"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, statement := range []string{"DELETE FROM user_entry_preferences WHERE user_id=1 AND (entry_id IN (2901,2903) OR entry_id BETWEEN 3000 AND 4000)", "DELETE FROM file_entries WHERE user_id=1 AND (id IN (2901,2903) OR id BETWEEN 3000 AND 4000)"} {
			if _, err := pool.ExecContext(cleanupContext, statement); err != nil {
				t.Error("清理名称预算夹具失败", err)
			}
		}
	}()
	for _, scope := range []string{"all", "starred"} {
		t.Run("budget empty continuation "+scope, func(t *testing.T) {
			listing, calls := makeList(nil)
			query := nameEntryCandidateQuery(2)
			query.Scope, query.Search, query.Kind = scope, "name-budget-", "document"
			first, err := listing.List(ctx, actor, query, nil)
			if err != nil || *calls != 10 {
				t.Fatal("名称预算扫描批次错误", err)
			}
			check(t, first, nil, 3999)
			next, err := listing.List(ctx, actor, query, signPosition(t, query, first.NextPosition))
			if err != nil || *calls != 11 {
				t.Fatal("名称预算续读错误", err)
			}
			check(t, next, []uint64{2901, 2903}, 0)
			cursor := checkTextPage(t, listing, query, "", nil, 3999)
			_ = checkTextPage(t, listing, query, cursor, []uint64{2901, 2903}, 0)
			if *calls != 22 {
				t.Fatal("名称文本预算分页未按十批加一批读取", *calls)
			}
		})
	}
	t.Run("short page and budget edge", func(t *testing.T) {
		defer restore("UPDATE file_entries SET parent_id=16 WHERE id IN (3000,3999)")
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=12 WHERE id=3000"); err != nil {
			t.Fatal(err)
		}
		listing, _ := makeList(nil)
		query := nameEntryCandidateQuery(3)
		query.Search = "name-budget-"
		first, err := listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, first, []uint64{3000}, 3999)
		next, err := listing.List(ctx, actor, query, signPosition(t, query, first.NextPosition))
		if err != nil {
			t.Fatal(err)
		}
		check(t, next, []uint64{2901, 2903}, 0)
		cursor := checkTextPage(t, listing, query, "", []uint64{3000}, 3999)
		_ = checkTextPage(t, listing, query, cursor, []uint64{2901, 2903}, 0)
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=IF(id=3999,12,16) WHERE id IN (3000,3999)"); err != nil {
			t.Fatal(err)
		}
		query.PageSize = 1
		first, err = listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, first, []uint64{3999}, 3999)
		_ = signPosition(t, query, first.NextPosition)
		_ = checkTextPage(t, listing, query, "", []uint64{3999}, 3999)
	})
	t.Run("exact budget end", func(t *testing.T) {
		defer restore("UPDATE file_entries SET status='ACTIVE',deleted_at=NULL WHERE id=4000")
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=4000"); err != nil {
			t.Fatal(err)
		}
		listing, calls := makeList(nil)
		query := nameEntryCandidateQuery(2)
		query.Search = "name-budget-hidden-"
		result, err := listing.List(ctx, actor, query, nil)
		if err != nil || *calls != 10 {
			t.Fatal("名称恰好预算结束错误", err)
		}
		check(t, result, nil, 0)
		_ = checkTextPage(t, listing, query, "", nil, 0)
		if *calls != 20 {
			t.Fatal("名称文本恰好预算结束的扫描批次错误", *calls)
		}
	})
}
