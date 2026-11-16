package repository

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	serverhttp "github.com/opaoxx/cloud-nest/internal/server/http"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/http/handler"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestGlobalEntryCursorSignerIntegration(t *testing.T) {
	// 1. 在正式表中准备预算空页及可见续页，装配全局列表和签名器。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash) VALUES(1,'fixture_one','全局签名用户','fixture_hash')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-global-cursor-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(12,1,11,'活动目录',CAST('活动目录' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(14,1,11,'回收目录',CAST('回收目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(13,1,12,'学习%_笔记.md',CAST('学习%_笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048),(15,1,12,'学习%_计划.md',CAST('学习%_计划.md' AS BINARY),'FILE','DOCUMENT',41,2048)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("建立全局签名夹具失败", err)
		}
	}
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=?", stamp); err != nil {
		t.Fatal(err)
	}
	writer, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	for index := 0; index < 1001; index++ {
		name := fmt.Sprintf("学习%%_被隐藏-%d.md", index)
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,modified_at) VALUES(?,1,14,?,?,'FILE','DOCUMENT',41,2048,?)", 1000+index, name, []byte(name), stamp.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	listing := service.NewGlobalEntryListService(func(current context.Context, action func(context.Context, service.GlobalEntryListReader) error) error {
		return store.WithFileEntrySnapshot(current, func(txContext context.Context, txStore *Store) error { return action(txContext, txStore) })
	})
	masterKey := bytes.Repeat([]byte{9}, 32)
	signer, err := service.NewGlobalEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewGlobalEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	query := globalEntryCandidateQuery(2)
	query.Search, query.Kind = "学习%_", "document"
	first, err := listing.List(ctx, actor, query, nil)
	if err != nil || first == nil || first.Entries == nil || len(first.Entries) != 0 || !first.HasMore || first.NextPosition == nil || first.NextPosition.ID != 1001 {
		t.Fatal("真实预算空页位置错误", err)
	}

	// 2. 对真实空页位置签名，独立核对载荷、摘要与 HMAC，并通过正式校验器还原。
	readPosition := func(cursor string) domain.FileEntryPosition {
		t.Helper()
		parts := strings.Split(cursor, ".")
		if len(parts) != 2 || len(cursor) > domain.MaxGlobalEntryCursorLength {
			t.Fatal("真实全局游标格式错误")
		}
		payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
		if err != nil {
			t.Fatal(err)
		}
		signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		derivation := hmac.New(sha256.New, masterKey)
		derivation.Write([]byte("cloudnest/global-entry-cursor/v1"))
		mac := hmac.New(sha256.New, derivation.Sum(nil))
		mac.Write([]byte(parts[0]))
		if !hmac.Equal(signature, mac.Sum(nil)) {
			t.Fatal("真实全局游标 HMAC 核对失败")
		}
		var payload struct {
			Version    uint8  `json:"version"`
			UserID     string `json:"user_id"`
			Scope      string `json:"scope"`
			Sort       string `json:"sort"`
			FilterHash string `json:"filter_hash"`
			ModifiedAt string `json:"modified_at"`
			EntryID    string `json:"entry_id"`
		}
		filters := sha256.Sum256([]byte(`{"q":"学习%_","kind":"document"}`))
		if json.Unmarshal(payloadBytes, &payload) != nil || payload.Version != 1 || payload.UserID != "1" || payload.Scope != "all" || payload.Sort != "modified_desc" || payload.FilterHash != hex.EncodeToString(filters[:]) {
			t.Fatal("真实全局载荷或筛选摘要错误")
		}
		entryID, err := strconv.ParseUint(payload.EntryID, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		modifiedAt, err := time.Parse("2006-01-02T15:04:05.000000Z", payload.ModifiedAt)
		if err != nil {
			t.Fatal(err)
		}
		position := domain.FileEntryPosition{ModifiedAt: modifiedAt, ID: entryID}
		if !position.IsValid() {
			t.Fatal("签名位置数据错误")
		}
		verified, err := verifier.Verify(actor.UserID, query, cursor)
		if err != nil || verified == nil || *verified != position {
			t.Fatal("正式校验器与独立解析的位置不一致", err)
		}
		return *verified
	}
	cursor, err := signer.Sign(actor.UserID, query, *first.NextPosition)
	if err != nil {
		t.Fatal(err)
	}
	position := readPosition(cursor)
	if position != *first.NextPosition {
		t.Fatal("签名未保留空页最后消费位置")
	}

	// 3. 从正式校验器还原的位置继续分页，验证页大小调整和稳定签名。
	query.PageSize = 1
	if resized, err := signer.Sign(actor.UserID, query, position); err != nil || resized != cursor {
		t.Fatal("合法页大小调整改变了查询绑定", err)
	}
	if verified, err := verifier.Verify(actor.UserID, query, cursor); err != nil || verified == nil || *verified != position {
		t.Fatal("调整合法页大小后校验失败", err)
	}
	second, err := listing.List(ctx, actor, query, &position)
	if err != nil || second == nil || len(second.Entries) != 1 || second.Entries[0].ID != 15 || !second.HasMore || second.NextPosition == nil || second.NextPosition.ID != 15 {
		t.Fatal("签名空页位置未衔接可见续页", err)
	}
	secondCursor, err := signer.Sign(actor.UserID, query, *second.NextPosition)
	if err != nil {
		t.Fatal(err)
	}
	secondPosition := readPosition(secondCursor)
	third, err := listing.List(ctx, actor, query, &secondPosition)
	if err != nil || third == nil || len(third.Entries) != 1 || third.Entries[0].ID != 13 || third.HasMore || third.NextPosition != nil {
		t.Fatal("真实签名续页漏项或结束状态错误", err)
	}
	recreated, err := service.NewGlobalEntryCursorSigner(bytes.Clone(masterKey))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := recreated.Sign(actor.UserID, query, position); err != nil || again != cursor {
		t.Fatal("相同主密钥重建后签名不稳定", err)
	}
	for _, mode := range []string{"user", "search", "kind", "tampering"} {
		userID, currentQuery, currentCursor := actor.UserID, query, cursor
		switch mode {
		case "user":
			userID = 3
		case "search":
			currentQuery.Search += " "
		case "kind":
			currentQuery.Kind = "image"
		case "tampering":
			currentCursor += "="
		}
		if verified, err := verifier.Verify(userID, currentQuery, currentCursor); verified != nil || !errors.Is(err, domain.ErrInvalidGlobalEntryCursor) {
			t.Fatal("真实页错误绑定或篡改游标被接受", err)
		}
	}

	// 4. 通过正式文本游标分页入口完成预算空页、正常续页和结束交付。
	listCalls := 0
	paging := service.NewGlobalEntryPageService(verifier.Verify,
		func(current context.Context, currentActor domain.Actor, filters domain.GlobalEntryQuery, before *domain.FileEntryPosition) (*domain.GlobalEntryList, error) {
			listCalls++
			return listing.List(current, currentActor, filters, before)
		}, signer.Sign)
	pageQuery := query
	pageQuery.PageSize = 2
	firstPage, err := paging.List(ctx, actor, pageQuery, "")
	if err != nil || firstPage == nil || firstPage.Entries == nil || len(firstPage.Entries) != 0 || firstPage.NextCursor == nil || *firstPage.NextCursor != cursor {
		t.Fatal("正式文本游标首页未保留预算空页位置", err)
	}
	pageQuery.PageSize = 1
	secondPage, err := paging.List(ctx, actor, pageQuery, *firstPage.NextCursor)
	if err != nil || secondPage == nil || len(secondPage.Entries) != 1 || secondPage.Entries[0].ID != 15 || secondPage.NextCursor == nil || *secondPage.NextCursor != secondCursor {
		t.Fatal("正式文本游标续页或页大小调整错误", err)
	}
	thirdPage, err := paging.List(ctx, actor, pageQuery, *secondPage.NextCursor)
	if err != nil || thirdPage == nil || len(thirdPage.Entries) != 1 || thirdPage.Entries[0].ID != 13 || thirdPage.NextCursor != nil || listCalls != 3 {
		t.Fatal("正式文本游标结束或调用次数错误", err)
	}
	// 5. 真实预算空页、正常续页和结束结果转换为公开 DTO。
	for _, page := range []*domain.GlobalEntryPageResult{firstPage, secondPage, thirdPage} {
		data, err := dto.NewGlobalEntryPageData(page)
		if err != nil || data == nil || data.Items == nil || data.Breadcrumbs == nil || len(data.Items) != len(page.Entries) || len(data.Breadcrumbs) != 0 || (data.NextCursor != nil) != (page.NextCursor != nil) {
			t.Fatal("真实全局分页 DTO 形状错误", err)
		}
		if page.NextCursor != nil && (*data.NextCursor != *page.NextCursor || data.NextCursor == page.NextCursor) {
			t.Fatal("真实全局游标转换或副本错误")
		}
		for index, item := range data.Items {
			entry := page.Entries[index]
			if item.ID != strconv.FormatUint(entry.ID, 10) || item.ParentID != strconv.FormatUint(*entry.ParentID, 10) || item.Name != entry.Name || item.SizeBytes != entry.FileSize || item.Status != "ACTIVE" || !item.ModifiedAt.Equal(entry.ModifiedAt) {
				t.Fatal("真实全局条目摘要字段错误")
			}
		}
		encoded, err := json.Marshal(dto.GlobalEntryPageResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"})
		if err != nil {
			t.Fatal(err)
		}
		var outer map[string]json.RawMessage
		var fields map[string]json.RawMessage
		if json.Unmarshal(encoded, &outer) != nil || len(outer) != 2 || json.Unmarshal(outer["data"], &fields) != nil || len(fields) != 3 || string(fields["breadcrumbs"]) != "[]" || (page.NextCursor == nil && string(fields["next_cursor"]) != "null") || (len(page.Entries) == 0 && string(fields["items"]) != "[]") {
			t.Fatal("真实全局响应 JSON 字段或空值错误")
		}
	}
	for _, mode := range []string{"user", "search", "kind", "tampering"} {
		currentActor, filters, currentCursor := actor, pageQuery, *firstPage.NextCursor
		switch mode {
		case "user":
			currentActor.UserID = 3
		case "search":
			filters.Search += " "
		case "kind":
			filters.Kind = "image"
		case "tampering":
			currentCursor += "="
		}
		result, err := paging.List(ctx, currentActor, filters, currentCursor)
		if result != nil || !errors.Is(err, domain.ErrInvalidGlobalEntryCursor) || listCalls != 3 {
			t.Fatal("错误文本游标进入了正式列表读取", err)
		}
	}

	// 6. 在空闲端口将真实预算空页、续页和结束结果交给 HTTP 成功方法。
	pages := map[string]*domain.GlobalEntryPageResult{"budget": firstPage, "next": secondPage, "end": thirdPage}
	var httpLogs bytes.Buffer
	router := serverhttp.NewRouter(logging.New(&httpLogs), nil, func(context.Context) bool { return true })
	router.GET("/test-global-entry-page-success/:stage", func(requestContext *gin.Context) {
		handler.GlobalEntryPageSuccess(requestContext, pages[requestContext.Param("stage")])
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	for _, stage := range []string{"budget", "next", "end"} {
		requestID := uuid.NewString()
		request, err := http.NewRequest("GET", server.URL+"/test-global-entry-page-success/"+stage+"?note=private-global-query", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-Request-ID", strings.ToUpper(requestID))
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		encoded, readError := io.ReadAll(response.Body)
		closeError := response.Body.Close()
		if readError != nil || closeError != nil || response.StatusCode != 200 || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Pragma") != "no-cache" || response.Header.Get("Content-Encoding") != "" || response.ContentLength != int64(len(encoded)) || len(response.Cookies()) != 0 {
			t.Fatal("真实数据库结果的 HTTP 响应头错误", readError, closeError)
		}
		var body dto.GlobalEntryPageResponse
		if json.Unmarshal(encoded, &body) != nil || body.RequestID != requestID || len(body.Data.Breadcrumbs) != 0 || body.Data.Items == nil || len(body.Data.Items) != len(pages[stage].Entries) || (body.Data.NextCursor != nil) != (pages[stage].NextCursor != nil) {
			t.Fatal("真实数据库结果的 HTTP JSON 错误")
		}
		if pages[stage].NextCursor != nil && *body.Data.NextCursor != *pages[stage].NextCursor {
			t.Fatal("真实预算或正常续页的游标丢失")
		}
	}
	server.Close()
	for _, marker := range []string{"private-global-query", "学习%_", cursor, secondCursor} {
		if strings.Contains(httpLogs.String(), marker) {
			t.Fatal("真实全局完成日志包含查询、名称或游标")
		}
	}
}
