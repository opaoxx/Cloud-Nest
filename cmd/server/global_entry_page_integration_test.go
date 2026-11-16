package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

// requestServerGlobalEntries 检查正式全局列表的实际 HTTP 响应及请求关联。
func requestServerGlobalEntries(t *testing.T, baseURL, token, raw string, status int, code, field string) dto.GlobalEntryPageResponse {
	t.Helper()
	// 1. 发送正式 GET，携带当前请求 ID 及额外身份字段。
	requestID := uuid.NewString()
	message, err := http.NewRequest("GET", baseURL+"/api/v1/entries?"+raw+"&user_id=3&parent_id=private-parent&note=private-global-query", nil)
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
		t.Fatal("正式全局列表请求失败", err)
	}
	encoded, readError := io.ReadAll(response.Body)
	closeError := response.Body.Close()
	if readError != nil || closeError != nil || response.StatusCode != status || response.Header.Get("X-Request-ID") != requestID || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Content-Encoding") != "" || response.Header.Get("Access-Control-Allow-Origin") != "http://127.0.0.1:5173" || response.ContentLength != int64(len(encoded)) || len(response.Cookies()) != 0 {
		t.Fatal("正式全局列表状态或响应头错误", response.StatusCode, readError, closeError)
	}

	// 2. 核对成功字段、预算空页或统一错误结构。
	var result dto.GlobalEntryPageResponse
	if status == 200 {
		var outer map[string]json.RawMessage
		var fields map[string]json.RawMessage
		if json.Unmarshal(encoded, &result) != nil || result.RequestID != requestID || result.Data.Items == nil || result.Data.Breadcrumbs == nil || len(result.Data.Breadcrumbs) != 0 || response.Header.Get("Pragma") != "no-cache" ||
			json.Unmarshal(encoded, &outer) != nil || len(outer) != 2 || json.Unmarshal(outer["data"], &fields) != nil || len(fields) != 3 || string(fields["breadcrumbs"]) != "[]" {
			t.Fatal("正式全局分页的 JSON 形状错误")
		}
		if result.Data.NextCursor == nil && string(fields["next_cursor"]) != "null" {
			t.Fatal("全局结束页游标未明确返回 null")
		}
		return result
	}
	var failure dto.ErrorResponse
	if json.Unmarshal(encoded, &failure) != nil || failure.Error.Code != code || failure.RequestID != requestID || strings.Contains(string(encoded), "private-") {
		t.Fatal("正式全局错误响应或原文保护错误")
	}
	if field == "" {
		if failure.Error.Details != nil {
			t.Fatal("普通全局错误 details 非空")
		}
	} else if len(failure.Error.Details) != 1 || failure.Error.Details["field"] != field {
		t.Fatal("正式全局错误字段错误")
	}
	if (status == 401) != (response.Header.Get("WWW-Authenticate") != "") {
		t.Fatal("正式全局认证提示错误")
	}
	return result
}

// verifyServerGlobalEntries 验证生产 run 装配的筛选、可见性和签名分页。
// 返回普通首页及预算首页，供服务重启后复用原始游标。
func verifyServerGlobalEntries(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, foreignToken string, signingKey []byte) (dto.GlobalEntryPageResponse, dto.GlobalEntryPageResponse) {
	t.Helper()
	// 1. 准备独立跨目录夹具及超过扫描预算的隐藏候选。
	for _, statement := range []string{
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(100041,3,'MINIO','fixture-bucket','fixture-global-server-foreign',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(100002,1,11,'全局父目录一',CAST('全局父目录一' AS BINARY),'DIR'),(100004,1,11,'全局父目录二',CAST('全局父目录二' AS BINARY),'DIR'),(100005,1,100002,'全局%_文件夹',CAST('全局%_文件夹' AS BINARY),'DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(100006,1,11,'全局回收目录',CAST('全局回收目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(100001,1,100002,'全局%_笔记.md',CAST('全局%_笔记.md' AS BINARY),'FILE','DOCUMENT',41,2048),(100003,1,100004,'全局%_计划.md',CAST('全局%_计划.md' AS BINARY),'FILE','DOCUMENT',41,2048),(100007,1,100006,'全局%_隐藏.md',CAST('全局%_隐藏.md' AS BINARY),'FILE','DOCUMENT',41,2048),(100008,1,100002,'预算%_可见.md',CAST('预算%_可见.md' AS BINARY),'FILE','DOCUMENT',41,2048),(100011,3,33,'全局%_其他.md',CAST('全局%_其他.md' AS BINARY),'FILE','DOCUMENT',100041,2048)",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET modified_at=? WHERE id BETWEEN 100001 AND 100011", stamp); err != nil {
		t.Fatal(err)
	}
	writer, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	for index := 0; index < 1001; index++ {
		name := fmt.Sprintf("预算%%_隐藏-%d.md", index)
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,modified_at) VALUES(?,1,100006,?,?,'FILE','DOCUMENT',41,2048,?)", 100100+index, name, []byte(name), stamp.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}

	// 2. 正式查询跨目录翻页，独立校验生产配置签发的游标位置。
	filters := "scope=all&kind=document&q=" + url.QueryEscape("全局%_")
	first := requestServerGlobalEntries(t, baseURL, token, filters+"&limit=1", 200, "", "")
	if len(first.Data.Items) != 1 || first.Data.Items[0].ID != "100003" || first.Data.Items[0].ParentID != "100004" || first.Data.NextCursor == nil {
		t.Fatal("正式全局首页或跨目录范围错误")
	}
	verifier, err := service.NewGlobalEntryCursorVerifier(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	query := domain.GlobalEntryQuery{Scope: "all", Sort: "modified_desc", Search: "全局%_", Kind: "document", PageSize: 1}
	position, err := verifier.Verify(1, query, *first.Data.NextCursor)
	if err != nil || position == nil || position.ID != 100003 || !position.ModifiedAt.Equal(stamp) {
		t.Fatal("正式配置的全局游标位置错误", err)
	}
	next := requestServerGlobalEntries(t, baseURL, token, filters+"&limit=2&cursor="+url.QueryEscape(*first.Data.NextCursor), 200, "", "")
	if len(next.Data.Items) != 1 || next.Data.Items[0].ID != "100001" || next.Data.Items[0].ParentID != "100002" || next.Data.NextCursor != nil {
		t.Fatal("正式全局续页或页大小调整错误")
	}
	allKinds := requestServerGlobalEntries(t, baseURL, token, "scope=all&q="+url.QueryEscape("全局%_"), 200, "", "")
	if len(allKinds.Data.Items) != 3 || allKinds.Data.Items[0].ID != "100005" || allKinds.Data.NextCursor != nil {
		t.Fatal("正式全局默认页大小、混合类型或隐藏过滤错误")
	}
	folders := requestServerGlobalEntries(t, baseURL, token, "scope=all&kind=folder&q="+url.QueryEscape("全局%_"), 200, "", "")
	if len(folders.Data.Items) != 1 || folders.Data.Items[0].ID != "100005" {
		t.Fatal("正式全局文件夹筛选错误")
	}
	empty := requestServerGlobalEntries(t, baseURL, token, "scope=all&kind=image&q="+url.QueryEscape("全局%_"), 200, "", "")
	if len(empty.Data.Items) != 0 || empty.Data.NextCursor != nil {
		t.Fatal("正式全局空结束页错误")
	}
	foreign := requestServerGlobalEntries(t, baseURL, foreignToken, filters, 200, "", "")
	if len(foreign.Data.Items) != 1 || foreign.Data.Items[0].ID != "100011" || foreign.Data.NextCursor != nil {
		t.Fatal("正式全局用户隔离错误")
	}

	// 3. 超过预算的隐藏候选交付空页，继续原游标后取得可见项。
	budgetFilters := "scope=all&kind=document&q=" + url.QueryEscape("预算%_")
	budget := requestServerGlobalEntries(t, baseURL, token, budgetFilters+"&limit=1", 200, "", "")
	if len(budget.Data.Items) != 0 || budget.Data.NextCursor == nil {
		t.Fatal("正式全局预算空页未保留游标")
	}
	query.Search = "预算%_"
	position, err = verifier.Verify(1, query, *budget.Data.NextCursor)
	if err != nil || position == nil || position.ID != 100101 || !position.ModifiedAt.Equal(stamp.Add(time.Second)) {
		t.Fatal("正式预算空页游标位置错误", err)
	}
	budgetNext := requestServerGlobalEntries(t, baseURL, token, budgetFilters+"&cursor="+url.QueryEscape(*budget.Data.NextCursor), 200, "", "")
	if len(budgetNext.Data.Items) != 1 || budgetNext.Data.Items[0].ID != "100008" || budgetNext.Data.NextCursor != nil {
		t.Fatal("正式预算空页续读错误")
	}

	// 4. 检查认证先行、字段错误及游标用户/筛选/用途绑定。
	requestServerGlobalEntries(t, baseURL, "", "scope=all&limit=0", 401, "AUTH_REQUIRED", "")
	requestServerGlobalEntries(t, baseURL, token, "scope=all&limit=0", 400, "INVALID_ARGUMENT", "limit")
	requestServerGlobalEntries(t, baseURL, token, "scope=all&q=%FF", 400, "INVALID_ARGUMENT", "q")
	requestServerGlobalEntries(t, baseURL, token, "scope=all&scope=all", 400, "INVALID_ARGUMENT", "scope")
	requestServerGlobalEntries(t, baseURL, token, "scope=all&note=%ZZ", 400, "INVALID_ARGUMENT", "query")
	requestServerGlobalEntries(t, baseURL, token, "scope=all&cursor=private-global-cursor", 400, "INVALID_CURSOR", "cursor")
	requestServerGlobalEntries(t, baseURL, foreignToken, filters+"&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	requestServerGlobalEntries(t, baseURL, token, "scope=all&kind=image&q="+url.QueryEscape("全局%_")+"&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	requestServerGlobalEntries(t, baseURL, token, filters+"+&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	directorySigner, err := service.NewDirectoryCursorSigner(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	directoryCursor, err := directorySigner.Sign(1, 12, domain.FileEntryPosition{ID: 23, ModifiedAt: stamp})
	if err != nil {
		t.Fatal(err)
	}
	requestServerGlobalEntries(t, baseURL, token, "scope=all&cursor="+url.QueryEscape(directoryCursor), 400, "INVALID_CURSOR", "cursor")
	return first, budget
}
