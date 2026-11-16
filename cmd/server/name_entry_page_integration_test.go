package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// verifyServerNameEntries 复用全局响应传输检查，验证正式名称分页与独立用途游标。
// 返回普通、收藏、预算及长键首页，供真实服务重启后继续读取。
func verifyServerNameEntries(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, foreignToken, globalCursor, recentCursor string, signingKey []byte) (dto.GlobalEntryPageResponse, dto.GlobalEntryPageResponse, dto.GlobalEntryPageResponse, dto.GlobalEntryPageResponse) {
	t.Helper()
	// 1. 复用已有普通及收藏夹具，核对实际名称排序和跨目录续读。
	filters := "scope=all&sort=name_asc&kind=document&q=" + url.QueryEscape("全局%_")
	first := requestServerGlobalEntries(t, baseURL, token, filters+"&limit=1", 200, "", "")
	if len(first.Data.Items) != 1 || first.Data.Items[0].ID != "100001" || first.Data.Items[0].ParentID != "100002" || first.Data.NextCursor == nil {
		t.Fatal("正式名称首页顺序或范围错误")
	}
	next := requestServerGlobalEntries(t, baseURL, token, filters+"&limit=2&cursor="+url.QueryEscape(*first.Data.NextCursor), 200, "", "")
	if len(next.Data.Items) != 1 || next.Data.Items[0].ID != "100003" || next.Data.Items[0].ParentID != "100004" || next.Data.NextCursor != nil {
		t.Fatal("正式名称跨目录续读错误")
	}
	starredFilters := "scope=starred&sort=name_asc&kind=document&q=" + url.QueryEscape("全局%_")
	starred := requestServerGlobalEntries(t, baseURL, token, starredFilters+"&limit=1", 200, "", "")
	if len(starred.Data.Items) != 1 || starred.Data.Items[0].ID != "100001" || starred.Data.NextCursor == nil {
		t.Fatal("正式名称收藏首页错误")
	}
	starredNext := requestServerGlobalEntries(t, baseURL, token, starredFilters+"&cursor="+url.QueryEscape(*starred.Data.NextCursor), 200, "", "")
	if len(starredNext.Data.Items) != 1 || starredNext.Data.Items[0].ID != "100003" || starredNext.Data.NextCursor != nil {
		t.Fatal("正式名称收藏续读错误")
	}
	encoded := requestServerGlobalEntries(t, baseURL, token, "%73cope=%61ll&%73ort=%6eame_asc&kind=document&q="+url.QueryEscape("全局%_")+"&limit=1", 200, "", "")
	if encoded.Data.NextCursor == nil || *encoded.Data.NextCursor != *first.Data.NextCursor {
		t.Fatal("正式名称编码参数或签名错误")
	}
	foreign := requestServerGlobalEntries(t, baseURL, foreignToken, filters, 200, "", "")
	if len(foreign.Data.Items) != 1 || foreign.Data.Items[0].ID != "100011" {
		t.Fatal("正式名称用户隔离错误")
	}
	folders := requestServerGlobalEntries(t, baseURL, token, "scope=all&sort=name_asc&kind=folder&q="+url.QueryEscape("全局%_"), 200, "", "")
	if len(folders.Data.Items) != 1 || folders.Data.Items[0].ID != "100005" {
		t.Fatal("正式名称文件夹筛选错误")
	}

	// 2. 两个跨目录原始二进制同键项，实际签名文本超过一千零二十四字节。
	for _, entry := range []struct{ entryID, parentID uint64 }{{300501, 100002}, {300503, 100004}} {
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,?,?,?,'FILE','DOCUMENT',41,2048)", entry.entryID, entry.parentID, fmt.Sprintf("name-long-%d.md", entry.entryID), bytes.Repeat([]byte{0xff}, 768)); err != nil {
			t.Fatal(err)
		}
		requestServerSetStarred(t, baseURL, token, fmt.Sprint(entry.entryID), `{"starred":true}`, 200, "", "")
	}
	var long dto.GlobalEntryPageResponse
	for _, scope := range []string{"all", "starred"} {
		query := "scope=" + scope + "&sort=name_asc&q=name-long-&kind=document&limit=1"
		page := requestServerGlobalEntries(t, baseURL, token, query, 200, "", "")
		if len(page.Data.Items) != 1 || page.Data.Items[0].ID != "300501" || page.Data.NextCursor == nil || len(*page.Data.NextCursor) <= 1024 || len(*page.Data.NextCursor) > domain.MaxNameEntryCursorLength {
			t.Fatal("正式名称长游标首页错误")
		}
		next := requestServerGlobalEntries(t, baseURL, token, query+"&cursor="+url.QueryEscape(*page.Data.NextCursor), 200, "", "")
		if len(next.Data.Items) != 1 || next.Data.Items[0].ID != "300503" || next.Data.NextCursor != nil {
			t.Fatal("正式名称同键长游标续读错误")
		}
		if scope == "all" {
			long = page
		}
	}

	// 3. 一千零一项隐藏候选后保存预算位置，再继续读取两项可见文件。
	writer, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	for index := 0; index < 1001; index++ {
		name := fmt.Sprintf("name-budget-%04d.md", index)
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,100006,?,?,'FILE','DOCUMENT',41,2048)", 301000+index, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct {
		entryID, parentID uint64
		name              string
	}{{300001, 100002, "name-budget-z.md"}, {300003, 100004, "name-budget-zz.md"}} {
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,?,?,?,'FILE','DOCUMENT',41,2048)", entry.entryID, entry.parentID, entry.name, []byte(entry.name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) SELECT 1,id,UTC_TIMESTAMP(6) FROM file_entries WHERE user_id=1 AND (id IN (300001,300003) OR id BETWEEN 301000 AND 302000)"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewNameEntryCursorVerifier(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	var budget dto.GlobalEntryPageResponse
	for _, scope := range []string{"all", "starred"} {
		query := "scope=" + scope + "&sort=name_asc&q=name-budget-&kind=document&limit=2"
		page := requestServerGlobalEntries(t, baseURL, token, query, 200, "", "")
		if len(page.Data.Items) != 0 || page.Data.NextCursor == nil {
			t.Fatal("正式名称预算空页错误")
		}
		position, err := verifier.Verify(1, domain.GlobalEntryQuery{Scope: scope, Sort: "name_asc", Search: "name-budget-", Kind: "document", PageSize: 2}, *page.Data.NextCursor)
		if err != nil || position == nil || position.ID != 301999 || string(position.NameKey) != "name-budget-0999.md" {
			t.Fatal("正式名称预算隐藏位置错误", err)
		}
		next := requestServerGlobalEntries(t, baseURL, token, query+"&cursor="+url.QueryEscape(*page.Data.NextCursor), 200, "", "")
		if len(next.Data.Items) != 2 || next.Data.Items[0].ID != "300001" || next.Data.Items[1].ID != "300003" || next.Data.NextCursor != nil {
			t.Fatal("正式名称预算空页续读错误")
		}
		if scope == "all" {
			budget = page
		}
	}

	// 4. 认证、字段、用户/范围/筛选和已有游标用途按正式入口验证。
	requestServerGlobalEntries(t, baseURL, "", "scope=all&sort=name_asc&limit=0", 401, "AUTH_REQUIRED", "")
	for _, invalid := range []struct{ raw, field string }{
		{"scope=all&sort=name_asc&limit=0", "limit"}, {"scope=all&sort=name_asc&sort=name_asc", "sort"},
		{"scope=all&sort=name_asc&q=%FF", "q"}, {"scope=all&sort=name_asc&cursor=" + string(bytes.Repeat([]byte{'a'}, 2049)), "cursor"},
	} {
		requestServerGlobalEntries(t, baseURL, token, invalid.raw, 400, "INVALID_ARGUMENT", invalid.field)
	}
	requestServerGlobalEntries(t, baseURL, foreignToken, filters+"&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	requestServerGlobalEntries(t, baseURL, token, starredFilters+"&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	requestServerGlobalEntries(t, baseURL, token, "scope=all&sort=name_asc&kind=image&q="+url.QueryEscape("全局%_")+"&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	requestServerGlobalEntries(t, baseURL, token, filters+"+&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	for _, cursor := range []string{globalCursor, recentCursor, "private-name-cursor"} {
		requestServerGlobalEntries(t, baseURL, token, filters+"&cursor="+url.QueryEscape(cursor), 400, "INVALID_CURSOR", "cursor")
	}
	requestServerGlobalEntries(t, baseURL, token, "scope=all&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	return first, starred, budget, long
}
