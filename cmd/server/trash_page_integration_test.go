package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
)

const serverTrashFirstQuery = "limit=1"

func requestServerTrashPage(t *testing.T, ctx context.Context, baseURL, token, query string, status int, code, field string) dto.TrashPageResponse {
	t.Helper()
	// 1. 通过正式 GET 发送分页请求。
	requestID := uuid.NewString()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/trash?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	const origin = "http://127.0.0.1:5173"
	request.Header.Set("X-Request-ID", strings.ToUpper(requestID))
	request.Header.Set("Origin", origin)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("正式回收站请求失败", err)
	}
	defer response.Body.Close()
	header := response.Header
	if response.StatusCode != status || header.Get("X-Request-ID") != requestID ||
		header.Get("Content-Type") != "application/json; charset=utf-8" || header.Get("Cache-Control") != "no-store" ||
		header.Get("Access-Control-Allow-Origin") != origin ||
		(status != http.StatusUnauthorized && header.Get("Pragma") != "no-cache") {
		t.Fatalf("状态为 %d，期望 %d，响应头为 %v", response.StatusCode, status, header)
	}

	// 2. 核对回收站公开响应、字段错误和本次请求 ID。
	decoder := json.NewDecoder(response.Body)
	if status == http.StatusOK {
		var page dto.TrashPageResponse
		if err := decoder.Decode(&page); err != nil || page.RequestID != requestID || page.Data.Items == nil {
			t.Fatalf("正式回收站分页结果为 %+v，错误为 %v", page, err)
		}
		return page
	}
	var failure dto.ErrorResponse
	if err := decoder.Decode(&failure); err != nil || failure.RequestID != requestID ||
		failure.Error.Code != code || (field != "" && failure.Error.Details["field"] != field) {
		t.Fatalf("正式回收站错误为 %+v，解码错误为 %v", failure, err)
	}
	return dto.TrashPageResponse{}
}

func verifyServerTrashPage(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, foreignToken string, directory dto.TrashedEntrySummary) (dto.TrashPageResponse, dto.TrashPageResponse, string) {
	t.Helper()
	// 1. 添加嵌套回收候选，确认正式入口按祖先状态隐藏它。
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at)
		VALUES (74005,1,74002,'嵌套回收项',CAST('嵌套回收项' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))`); err != nil {
		t.Fatal("准备嵌套回收候选失败", err)
	}
	requestServerTrashPage(t, ctx, baseURL, "", "limit=0", 401, "AUTH_REQUIRED", "")
	requestServerTrashPage(t, ctx, baseURL, token, "limit=0", 400, "INVALID_ARGUMENT", "limit")
	requestServerTrashPage(t, ctx, baseURL, token, "cursor=invalid", 400, "INVALID_CURSOR", "cursor")

	// 2. 核对本次正式回收产生的目录在首页、文件在续页。
	first := requestServerTrashPage(t, ctx, baseURL, token, serverTrashFirstQuery, 200, "", "")
	if len(first.Data.Items) != 1 || first.Data.Items[0].TrashedEntrySummary != directory || first.Data.NextCursor == nil {
		t.Fatalf("正式回收站首页为 %+v", first.Data)
	}
	nextQuery := url.Values{"limit": {"100"}, "cursor": {*first.Data.NextCursor}}.Encode()
	requestServerTrashPage(t, ctx, baseURL, foreignToken, nextQuery, 400, "INVALID_CURSOR", "cursor")
	next := requestServerTrashPage(t, ctx, baseURL, token, nextQuery, 200, "", "")
	if next.Data.NextCursor != nil || len(next.Data.Items) == 0 {
		t.Fatalf("正式回收站末页为 %+v", next.Data)
	}
	var foundFile bool
	for _, item := range next.Data.Items {
		switch item.ID {
		case serverTrashDirectoryID, "74005":
			t.Fatal("续页重复展示首页目录或展示了嵌套回收候选")
		case "74004":
			foundFile = true
			if item.ParentID != "74001" || item.EntryType != "FILE" || item.SizeBytes != 2048 || item.Revision != "4" ||
				len(item.OriginalPath) != 2 || item.OriginalPath[0] != (dto.DirectoryBreadcrumb{ID: "11"}) ||
				item.OriginalPath[1] != (dto.DirectoryBreadcrumb{ID: "74001", Name: "回收测试目录"}) ||
				!item.RetentionExpiresAt.Equal(item.DeletedAt.AddDate(0, 0, 30)) {
				t.Fatalf("正式回收站文件数据为 %+v", item)
			}
		}
	}
	if !foundFile {
		t.Fatal("正式回收后的文件未出现在续页")
	}
	return first, next, nextQuery
}
