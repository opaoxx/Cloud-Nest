package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// verifyServerSizeEntries 复用十字段响应传输检查，验证正式大小分页及独立用途游标。
// 返回普通、收藏、预算及同大小首页，供真实服务重启后继续读取。
func verifyServerSizeEntries(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, foreignToken, globalCursor, recentCursor, nameCursor string, signingKey []byte) (dto.GlobalEntryPageResponse, dto.GlobalEntryPageResponse, dto.GlobalEntryPageResponse, dto.GlobalEntryPageResponse) {
	t.Helper()
	// 1. 复用跨目录夹具，按大小和 ID 双降序验证普通及收藏范围。
	filters := "scope=all&sort=size_desc&kind=document&q=" + url.QueryEscape("全局%_")
	first := requestServerGlobalEntries(t, baseURL, token, filters+"&limit=1", 200, "", "")
	if len(first.Data.Items) != 1 || first.Data.Items[0].ID != "100003" || first.Data.Items[0].ParentID != "100004" || first.Data.NextCursor == nil {
		t.Fatal("正式大小首页或跨目录范围错误")
	}
	next := requestServerGlobalEntries(t, baseURL, token, filters+"&limit=100&cursor="+url.QueryEscape(*first.Data.NextCursor), 200, "", "")
	if len(next.Data.Items) != 1 || next.Data.Items[0].ID != "100001" || next.Data.NextCursor != nil {
		t.Fatal("正式大小原游标续页或页大小调整错误")
	}
	starredFilters := "scope=starred&sort=size_desc&kind=document&q=" + url.QueryEscape("全局%_")
	starred := requestServerGlobalEntries(t, baseURL, token, starredFilters+"&limit=1", 200, "", "")
	if len(starred.Data.Items) != 1 || starred.Data.Items[0].ID != "100003" || starred.Data.NextCursor == nil {
		t.Fatal("正式大小收藏首页错误")
	}
	starredNext := requestServerGlobalEntries(t, baseURL, token, starredFilters+"&cursor="+url.QueryEscape(*starred.Data.NextCursor), 200, "", "")
	if len(starredNext.Data.Items) != 1 || starredNext.Data.Items[0].ID != "100001" || starredNext.Data.NextCursor != nil {
		t.Fatal("正式大小收藏续页错误")
	}
	encoded := requestServerGlobalEntries(t, baseURL, token, "%73cope=%61ll&%73ort=%73ize_desc&kind=document&q="+url.QueryEscape("全局%_")+"&limit=1", 200, "", "")
	if encoded.Data.NextCursor == nil || *encoded.Data.NextCursor != *first.Data.NextCursor {
		t.Fatal("正式大小编码参数或签名错误")
	}
	foreign := requestServerGlobalEntries(t, baseURL, foreignToken, filters, 200, "", "")
	if len(foreign.Data.Items) != 1 || foreign.Data.Items[0].ID != "100011" {
		t.Fatal("正式大小用户隔离错误")
	}
	folders := requestServerGlobalEntries(t, baseURL, token, "scope=all&sort=size_desc&kind=folder&q="+url.QueryEscape("全局%_"), 200, "", "")
	if len(folders.Data.Items) != 1 || folders.Data.Items[0].ID != "100005" || folders.Data.Items[0].SizeBytes != 0 {
		t.Fatal("正式大小目录零字节筛选错误")
	}

	// 2. 两个同大小文件和零字节文件，使用正式收藏写入再读取。
	for _, entry := range []struct{ entryID, parentID uint64 }{{400501, 100002}, {400503, 100004}} {
		name := fmt.Sprintf("size-tie-%d.md", entry.entryID)
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,?,?,?,'FILE','DOCUMENT',41,8192)", entry.entryID, entry.parentID, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
		requestServerSetStarred(t, baseURL, token, fmt.Sprint(entry.entryID), `{"starred":true}`, 200, "", "")
	}
	if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(400505,1,100002,'size-zero.bin',CAST('size-zero.bin' AS BINARY),'FILE','OTHER',41,0)"); err != nil {
		t.Fatal(err)
	}
	zero := requestServerGlobalEntries(t, baseURL, token, "scope=all&sort=size_desc&q=size-zero&kind=other", 200, "", "")
	if len(zero.Data.Items) != 1 || zero.Data.Items[0].ID != "400505" || zero.Data.Items[0].EntryType != "FILE" || zero.Data.Items[0].SizeBytes != 0 || zero.Data.NextCursor != nil {
		t.Fatal("正式大小零字节文件错误")
	}
	var tie dto.GlobalEntryPageResponse
	for _, scope := range []string{"all", "starred"} {
		query := "scope=" + scope + "&sort=size_desc&q=size-tie-&kind=document&limit=1"
		page := requestServerGlobalEntries(t, baseURL, token, query, 200, "", "")
		if len(page.Data.Items) != 1 || page.Data.Items[0].ID != "400503" || page.Data.Items[0].SizeBytes != 8192 || page.Data.NextCursor == nil || len(*page.Data.NextCursor) > domain.MaxSizeEntryCursorLength {
			t.Fatal("正式大小同值首页错误")
		}
		next := requestServerGlobalEntries(t, baseURL, token, query+"&cursor="+url.QueryEscape(*page.Data.NextCursor), 200, "", "")
		if len(next.Data.Items) != 1 || next.Data.Items[0].ID != "400501" || next.Data.NextCursor != nil {
			t.Fatal("正式大小同值续页错误")
		}
		if scope == "all" {
			tie = page
		}
	}

	// 3. 一千零一项大文件隐藏候选后，继续读取更小的可见文件。
	writer, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	for index := 0; index < 1001; index++ {
		name := fmt.Sprintf("size-budget-hidden-%04d.md", index)
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,100006,?,?,'FILE','DOCUMENT',41,16384)", 401000+index, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct {
		entryID, parentID, size uint64
		name                    string
	}{
		{400001, 100002, 1024, "size-budget-visible-a.md"}, {400003, 100004, 512, "size-budget-visible-b.md"},
	} {
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,?,?,?,'FILE','DOCUMENT',41,?)", entry.entryID, entry.parentID, entry.name, []byte(entry.name), entry.size); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) SELECT 1,id,UTC_TIMESTAMP(6) FROM file_entries WHERE user_id=1 AND (id IN (400001,400003) OR id BETWEEN 401000 AND 402000)"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewSizeEntryCursorVerifier(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	var budget dto.GlobalEntryPageResponse
	for _, scope := range []string{"all", "starred"} {
		query := "scope=" + scope + "&sort=size_desc&q=size-budget-&kind=document&limit=2"
		page := requestServerGlobalEntries(t, baseURL, token, query, 200, "", "")
		if len(page.Data.Items) != 0 || page.Data.NextCursor == nil {
			t.Fatal("正式大小预算空页错误")
		}
		position, err := verifier.Verify(1, domain.GlobalEntryQuery{Scope: scope, Sort: "size_desc", Search: "size-budget-", Kind: "document", PageSize: 2}, *page.Data.NextCursor)
		if err != nil || position == nil || position.ID != 401001 || position.FileSize != 16384 {
			t.Fatal("正式大小预算位置错误", err)
		}
		next := requestServerGlobalEntries(t, baseURL, token, query+"&cursor="+url.QueryEscape(*page.Data.NextCursor), 200, "", "")
		if len(next.Data.Items) != 2 || next.Data.Items[0].ID != "400001" || next.Data.Items[0].SizeBytes != 1024 || next.Data.Items[1].ID != "400003" || next.Data.Items[1].SizeBytes != 512 || next.Data.NextCursor != nil {
			t.Fatal("正式大小预算空页续读错误")
		}
		if scope == "all" {
			budget = page
		}
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=100002 WHERE id=402000"); err != nil {
		t.Fatal(err)
	}
	shortQuery := "scope=all&sort=size_desc&q=size-budget-&kind=document&limit=3"
	short := requestServerGlobalEntries(t, baseURL, token, shortQuery, 200, "", "")
	if len(short.Data.Items) != 1 || short.Data.Items[0].ID != "402000" || short.Data.NextCursor == nil {
		t.Fatal("正式大小预算短页错误")
	}
	shortNext := requestServerGlobalEntries(t, baseURL, token, shortQuery+"&cursor="+url.QueryEscape(*short.Data.NextCursor), 200, "", "")
	if len(shortNext.Data.Items) != 2 || shortNext.Data.Items[0].ID != "400001" || shortNext.Data.Items[1].ID != "400003" || shortNext.Data.NextCursor != nil {
		t.Fatal("正式大小预算短页续读错误")
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=100006 WHERE id=402000"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=401000"); err != nil {
		t.Fatal(err)
	}
	exact := requestServerGlobalEntries(t, baseURL, token, "scope=all&sort=size_desc&q=size-budget-hidden-&kind=document&limit=2", 200, "", "")
	if len(exact.Data.Items) != 0 || exact.Data.NextCursor != nil {
		t.Fatal("正式大小恰好预算结束错误")
	}
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='ACTIVE',deleted_at=NULL WHERE id=401000"); err != nil {
		t.Fatal(err)
	}

	// 4. 认证、字段、用户/范围/筛选及其他排序用途在正式入口验证。
	requestServerGlobalEntries(t, baseURL, "", "scope=all&sort=size_desc&limit=0", 401, "AUTH_REQUIRED", "")
	for _, invalid := range []struct{ raw, field string }{
		{"scope=all&sort=size_desc&limit=0", "limit"}, {"scope=all&sort=size_desc&sort=size_desc", "sort"},
		{"scope=all&sort=size_desc&q=%FF", "q"}, {"scope=all&sort=size_desc&cursor=" + strings.Repeat("a", 1025), "cursor"},
	} {
		requestServerGlobalEntries(t, baseURL, token, invalid.raw, 400, "INVALID_ARGUMENT", invalid.field)
	}
	requestServerGlobalEntries(t, baseURL, foreignToken, filters+"&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	requestServerGlobalEntries(t, baseURL, token, starredFilters+"&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	requestServerGlobalEntries(t, baseURL, token, "scope=all&sort=size_desc&kind=image&q="+url.QueryEscape("全局%_")+"&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	requestServerGlobalEntries(t, baseURL, token, filters+"+&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	directorySigner, err := service.NewDirectoryCursorSigner(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	directoryCursor, err := directorySigner.Sign(1, 100002, domain.FileEntryPosition{ID: 100001, ModifiedAt: time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	for _, cursor := range []string{globalCursor, recentCursor, nameCursor, directoryCursor, "private-size-cursor"} {
		requestServerGlobalEntries(t, baseURL, token, filters+"&cursor="+url.QueryEscape(cursor), 400, "INVALID_CURSOR", "cursor")
	}
	requestServerGlobalEntries(t, baseURL, token, "scope=all&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	return first, starred, budget, tie
}
