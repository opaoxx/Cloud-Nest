package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// requestServerRecentEntries 检查正式最近使用列表的实际 HTTP 响应及请求关联。
func requestServerRecentEntries(t *testing.T, baseURL, token, raw string, status int, code, field string) dto.RecentEntryPageResponse {
	t.Helper()
	// 1. 发送正式 GET，携带当前请求 ID 及额外身份字段。
	requestID := uuid.NewString()
	message, err := http.NewRequest("GET", baseURL+"/api/v1/entries?"+raw+"&user_id=3&parent_id=private-parent&note=private-recent-query", nil)
	if err != nil {
		t.Fatal(err)
	}
	message.Header.Set("Origin", "http://127.0.0.1:5173")
	message.Header.Set("X-Request-ID", strings.ToUpper(requestID))
	if token != "" {
		message.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(message)
	if err != nil {
		t.Fatal("正式最近使用列表请求失败", err)
	}
	encoded, readError := io.ReadAll(response.Body)
	closeError := response.Body.Close()
	if readError != nil || closeError != nil || response.StatusCode != status || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Encoding") != "" || response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || response.ContentLength != int64(len(encoded)) || len(response.Cookies()) != 0 {
		t.Fatal("正式最近使用列表状态或响应头错误", response.StatusCode, readError, closeError)
	}

	// 2. 核对成功字段、预算空页或统一错误结构。
	var result dto.RecentEntryPageResponse
	if status == 200 {
		var outer map[string]json.RawMessage
		var fields map[string]json.RawMessage
		if json.Unmarshal(encoded, &result) != nil || result.RequestID != requestID || result.Data.Items == nil || result.Data.Breadcrumbs == nil || len(result.Data.Breadcrumbs) != 0 || response.Header.Get("Pragma") != "no-cache" ||
			json.Unmarshal(encoded, &outer) != nil || len(outer) != 2 || json.Unmarshal(outer["data"], &fields) != nil || len(fields) != 3 || string(fields["breadcrumbs"]) != "[]" {
			t.Fatal("正式最近使用分页的 JSON 形状错误")
		}
		var items []map[string]json.RawMessage
		if json.Unmarshal(fields["items"], &items) != nil || items == nil {
			t.Fatal("最近文件数组编码错误")
		}
		for index, item := range result.Data.Items {
			if len(items[index]) != 11 || item.EntryType != "FILE" || item.LastOpenedAt.IsZero() || item.LastOpenedAt.Location() != time.UTC || item.LastOpenedAt.Nanosecond()%1000 != 0 {
				t.Fatal("最近文件公开字段或访问时间错误")
			}
		}
		if result.Data.NextCursor == nil && string(fields["next_cursor"]) != "null" {
			t.Fatal("最近使用结束页游标未明确返回 null")
		}
		return result
	}
	var failure dto.ErrorResponse
	if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != requestID || strings.Contains(string(encoded), "private-") {
		t.Fatal("正式最近使用错误响应或原文保护错误")
	}
	if field == "" {
		if failure.Error.Details != nil {
			t.Fatal("普通最近使用错误 details 非空")
		}
	} else if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
		t.Fatal("正式最近使用错误字段错误")
	}
	if (status == 401) != (response.Header.Get("WWW-Authenticate") != "") {
		t.Fatal("正式最近使用认证提示错误")
	}
	return result
}

// verifyServerRecentEntries 验证正式访问写入、最近列表、预算和独立用途游标。
// 返回普通和预算首页，供真实服务重启后沿用原游标。
func verifyServerRecentEntries(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, foreignToken, globalCursor, starredCursor string, signingKey []byte) (dto.RecentEntryPageResponse, dto.RecentEntryPageResponse) {
	t.Helper()
	// 1. 通过正式访问入口建立可见记录，并立即对照最近列表。
	for _, id := range []string{"100001", "100003", "100008"} {
		accessed := requestServerRecordAccess(t, baseURL, token, id, `{"purpose":"open"}`, 200, "", "")
		if id == "100003" {
			page := requestServerRecentEntries(t, baseURL, token, "scope=recent&q="+url.QueryEscape("全局%_计划"), 200, "", "")
			if len(page.Data.Items) != 1 || page.Data.Items[0].ID != id || !page.Data.Items[0].LastOpenedAt.Equal(accessed.Data.LastOpenedAt) {
				t.Fatal("正式访问未衔接最近列表")
			}
		}
	}
	requestServerRecordAccess(t, baseURL, foreignToken, "100011", `{"purpose":"preview"}`, 200, "", "")
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	for _, entry := range []struct {
		id     uint64
		offset time.Duration
	}{{100001, time.Second}, {100003, 2 * time.Second}, {100008, -time.Second}, {100011, 0}, {11, 4 * time.Second}, {100005, 4 * time.Second}} {
		if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=? WHERE entry_id=?", stamp.Add(entry.offset), entry.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET last_opened_at=? WHERE user_id=1 AND (entry_id=100007 OR entry_id BETWEEN 100100 AND 101100)", stamp.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(200009,1,100002,'最近专用未访问.md',CAST('最近专用未访问.md' AS BINARY),'FILE','DOCUMENT',41,2048)"); err != nil {
		t.Fatal(err)
	}

	// 2. 核对访问排序、跨目录分页、筛选交集与用户隔离。
	filters := "scope=recent&kind=document&q=" + url.QueryEscape("全局%_")
	first := requestServerRecentEntries(t, baseURL, token, filters+"&limit=1", 200, "", "")
	if len(first.Data.Items) != 1 || first.Data.Items[0].ID != "100003" || first.Data.Items[0].ParentID != "100004" || first.Data.NextCursor == nil || !first.Data.Items[0].LastOpenedAt.Equal(stamp.Add(2*time.Second)) {
		t.Fatal("正式最近首页或访问排序错误")
	}
	next := requestServerRecentEntries(t, baseURL, token, filters+"&limit=2&cursor="+url.QueryEscape(*first.Data.NextCursor), 200, "", "")
	if len(next.Data.Items) != 1 || next.Data.Items[0].ID != "100001" || next.Data.Items[0].ParentID != "100002" || next.Data.NextCursor != nil {
		t.Fatal("正式最近续页或页大小调整错误")
	}
	encoded := requestServerRecentEntries(t, baseURL, token, "%73cope=%72ecent&kind=document&q="+url.QueryEscape("全局%_")+"&limit=1", 200, "", "")
	if encoded.Data.NextCursor == nil || *encoded.Data.NextCursor != *first.Data.NextCursor {
		t.Fatal("编码后的最近范围错误")
	}
	for _, raw := range []string{"scope=recent&kind=folder", "scope=recent&kind=image&q=" + url.QueryEscape("全局%_"), "scope=recent&q=" + url.QueryEscape("最近专用未访问")} {
		empty := requestServerRecentEntries(t, baseURL, token, raw, 200, "", "")
		if len(empty.Data.Items) != 0 || empty.Data.NextCursor != nil {
			t.Fatal("最近空交集或未访问排除错误")
		}
	}
	foreign := requestServerRecentEntries(t, baseURL, foreignToken, filters, 200, "", "")
	if len(foreign.Data.Items) != 1 || foreign.Data.Items[0].ID != "100011" {
		t.Fatal("最近用户隔离错误")
	}

	// 3. 核对预算空页的最后隐藏位置，并使用原签名续读。
	budgetFilters := "scope=recent&kind=document&q=" + url.QueryEscape("预算%_")
	budget := requestServerRecentEntries(t, baseURL, token, budgetFilters+"&limit=1", 200, "", "")
	if len(budget.Data.Items) != 0 || budget.Data.NextCursor == nil {
		t.Fatal("最近预算空页未保留游标")
	}
	verifier, err := service.NewRecentEntryCursorVerifier(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	query := domain.GlobalEntryQuery{Scope: "recent", Sort: "last_opened_desc", Search: "预算%_", Kind: "document", PageSize: 1}
	position, err := verifier.Verify(1, query, *budget.Data.NextCursor)
	if err != nil || position == nil || position.ID != 100101 || !position.LastOpenedAt.Equal(stamp.Add(3*time.Second)) {
		t.Fatal("最近预算最后扫描位置错误", err)
	}
	budgetNext := requestServerRecentEntries(t, baseURL, token, budgetFilters+"&cursor="+url.QueryEscape(*budget.Data.NextCursor), 200, "", "")
	if len(budgetNext.Data.Items) != 1 || budgetNext.Data.Items[0].ID != "100008" || budgetNext.Data.NextCursor != nil {
		t.Fatal("最近预算续读错误")
	}

	// 4. 验证认证先行、固定参数错误和游标的用户/筛选/用途绑定。
	requestServerRecentEntries(t, baseURL, "", "scope=recent&limit=0", 401, "AUTH_REQUIRED", "")
	for _, invalid := range []struct{ raw, field string }{{"scope=recent&limit=0", "limit"}, {"scope=recent&scope=recent", "scope"}, {"scope=recent&sort=modified_desc", "sort"}, {"scope=recent&q=%FF", "q"}, {"scope=recent&note=%ZZ", "query"}} {
		requestServerRecentEntries(t, baseURL, token, invalid.raw, 400, "INVALID_ARGUMENT", invalid.field)
	}
	requestServerRecentEntries(t, baseURL, token, "scope=recent&cursor=private-recent-cursor", 400, "INVALID_CURSOR", "cursor")
	requestServerRecentEntries(t, baseURL, foreignToken, filters+"&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	requestServerRecentEntries(t, baseURL, token, "scope=recent&kind=image&q="+url.QueryEscape("全局%_")+"&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	requestServerRecentEntries(t, baseURL, token, filters+"+&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	for _, cursor := range []string{globalCursor, starredCursor} {
		requestServerRecentEntries(t, baseURL, token, filters+"&cursor="+url.QueryEscape(cursor), 400, "INVALID_CURSOR", "cursor")
	}
	requestServerGlobalEntries(t, baseURL, token, "scope=all&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	return first, budget
}
