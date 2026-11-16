package main

import (
	"context"
	"database/sql"
	"net/url"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

// verifyServerStarredEntries 复用全局夹具，验证正式收藏写入、查询及签名续页。
// 返回普通首页和预算首页，供真实服务重启后继续使用原游标。
func verifyServerStarredEntries(t *testing.T, ctx context.Context, pool *sql.DB, baseURL, token, foreignToken, globalCursor string, signingKey []byte) (dto.GlobalEntryPageResponse, dto.GlobalEntryPageResponse) {
	t.Helper()
	// 1. 通过正式 PUT 建立可见收藏，补齐回收路径下的历史收藏夹具。
	for _, id := range []string{"100001", "100003", "100008", "11"} {
		result := requestServerSetStarred(t, baseURL, token, id, `{"starred":true}`, 200, "", "")
		if !result.Data.Starred || result.Data.StarredAt == nil {
			t.Fatal("正式收藏未保存时间")
		}
	}
	requestServerSetStarred(t, baseURL, foreignToken, "100011", `{"starred":true}`, 200, "", "")
	stamp := time.Date(2026, 11, 16, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) SELECT user_id,id,? FROM file_entries WHERE user_id=1 AND (id=100007 OR id BETWEEN 100100 AND 101100)", stamp); err != nil {
		t.Fatal("建立隐藏收藏夹具失败", err)
	}
	filters := "scope=starred&kind=document&q=" + url.QueryEscape("全局%_")

	// 2. 验证跨目录分页、编码范围、用户隔离及收藏状态变化。
	first := requestServerGlobalEntries(t, baseURL, token, filters+"&limit=1", 200, "", "")
	if len(first.Data.Items) != 1 || first.Data.Items[0].ID != "100003" || first.Data.Items[0].ParentID != "100004" || first.Data.NextCursor == nil {
		t.Fatal("正式收藏首页、隐藏过滤或排序错误")
	}
	next := requestServerGlobalEntries(t, baseURL, token, filters+"&limit=2&cursor="+url.QueryEscape(*first.Data.NextCursor), 200, "", "")
	if len(next.Data.Items) != 1 || next.Data.Items[0].ID != "100001" || next.Data.Items[0].ParentID != "100002" || next.Data.NextCursor != nil {
		t.Fatal("正式收藏跨目录续页错误")
	}
	encoded := requestServerGlobalEntries(t, baseURL, token, "%73cope=%73tarred&kind=document&q="+url.QueryEscape("全局%_")+"&limit=1", 200, "", "")
	if len(encoded.Data.Items) != 1 || encoded.Data.Items[0].ID != "100003" || encoded.Data.NextCursor == nil || *encoded.Data.NextCursor != *first.Data.NextCursor {
		t.Fatal("编码后的正式收藏范围错误")
	}
	mixed := requestServerGlobalEntries(t, baseURL, token, "scope=starred&q="+url.QueryEscape("全局%_"), 200, "", "")
	if len(mixed.Data.Items) != 2 || mixed.Data.NextCursor != nil {
		t.Fatal("未收藏目录项进入了收藏结果")
	}
	folderQuery := "scope=starred&kind=folder&q=" + url.QueryEscape("全局%_")
	requestServerSetStarred(t, baseURL, token, "100005", `{"starred":true}`, 200, "", "")
	folder := requestServerGlobalEntries(t, baseURL, token, folderQuery, 200, "", "")
	if len(folder.Data.Items) != 1 || folder.Data.Items[0].ID != "100005" || folder.Data.NextCursor != nil {
		t.Fatal("正式收藏文件夹未进入分类列表")
	}
	requestServerSetStarred(t, baseURL, token, "100005", `{"starred":false}`, 200, "", "")
	emptyFolder := requestServerGlobalEntries(t, baseURL, token, folderQuery, 200, "", "")
	if len(emptyFolder.Data.Items) != 0 || emptyFolder.Data.NextCursor != nil {
		t.Fatal("取消收藏后文件夹仍在收藏列表")
	}
	requestServerSetStarred(t, baseURL, token, "100005", `{"starred":true}`, 200, "", "")
	foreign := requestServerGlobalEntries(t, baseURL, foreignToken, filters, 200, "", "")
	if len(foreign.Data.Items) != 1 || foreign.Data.Items[0].ID != "100011" || foreign.Data.NextCursor != nil {
		t.Fatal("正式收藏用户隔离错误")
	}
	empty := requestServerGlobalEntries(t, baseURL, token, "scope=starred&kind=image&q="+url.QueryEscape("全局%_"), 200, "", "")
	if len(empty.Data.Items) != 0 || empty.Data.NextCursor != nil {
		t.Fatal("正式收藏空结束页错误")
	}

	// 3. 使用生产密钥核对普通及预算空页的最后消费位置。
	verifier, err := service.NewGlobalEntryCursorVerifier(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	query := domain.GlobalEntryQuery{Scope: "starred", Sort: "modified_desc", Search: "全局%_", Kind: "document", PageSize: 1}
	position, err := verifier.Verify(1, query, *first.Data.NextCursor)
	if err != nil || position == nil || position.ID != 100003 || !position.ModifiedAt.Equal(stamp) {
		t.Fatal("正式收藏签名位置错误", err)
	}
	budgetFilters := "scope=starred&kind=document&q=" + url.QueryEscape("预算%_")
	budget := requestServerGlobalEntries(t, baseURL, token, budgetFilters+"&limit=1", 200, "", "")
	if len(budget.Data.Items) != 0 || budget.Data.NextCursor == nil {
		t.Fatal("正式收藏预算空页丢失续页文本")
	}
	query.Search = "预算%_"
	position, err = verifier.Verify(1, query, *budget.Data.NextCursor)
	if err != nil || position == nil || position.ID != 100101 || !position.ModifiedAt.Equal(stamp.Add(time.Second)) {
		t.Fatal("正式收藏预算空页位置错误", err)
	}
	budgetNext := requestServerGlobalEntries(t, baseURL, token, budgetFilters+"&cursor="+url.QueryEscape(*budget.Data.NextCursor), 200, "", "")
	if len(budgetNext.Data.Items) != 1 || budgetNext.Data.Items[0].ID != "100008" || budgetNext.Data.NextCursor != nil {
		t.Fatal("正式收藏预算空页续读错误")
	}

	// 4. 核对认证先行、字段错误及用户/范围/筛选/用途绑定。
	requestServerGlobalEntries(t, baseURL, "", "scope=starred&limit=0", 401, "AUTH_REQUIRED", "")
	requestServerGlobalEntries(t, baseURL, token, "scope=starred&limit=0", 400, "INVALID_ARGUMENT", "limit")
	requestServerGlobalEntries(t, baseURL, token, "scope=starred&scope=starred", 400, "INVALID_ARGUMENT", "scope")
	requestServerGlobalEntries(t, baseURL, token, "scope=starred&q=%FF", 400, "INVALID_ARGUMENT", "q")
	requestServerGlobalEntries(t, baseURL, token, "scope=starred&cursor=private-starred-cursor", 400, "INVALID_CURSOR", "cursor")
	requestServerGlobalEntries(t, baseURL, foreignToken, filters+"&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	requestServerGlobalEntries(t, baseURL, token, "scope=starred&kind=image&q="+url.QueryEscape("全局%_")+"&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	requestServerGlobalEntries(t, baseURL, token, filters+"+&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	requestServerGlobalEntries(t, baseURL, token, "scope=all&kind=document&q="+url.QueryEscape("全局%_")+"&cursor="+url.QueryEscape(*first.Data.NextCursor), 400, "INVALID_CURSOR", "cursor")
	requestServerGlobalEntries(t, baseURL, token, filters+"&cursor="+url.QueryEscape(globalCursor), 400, "INVALID_CURSOR", "cursor")
	return first, budget
}
