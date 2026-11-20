package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"golang.org/x/crypto/bcrypt"
)

const webLoginFixtureUsername = "fixture_web_login"
const webLoginFixturePassword = "fixture-web-login-secret"
const webLoginFixtureDisplayName = "隔离登录测试用户"
const webEmptyRootFixtureUsername = "fixture_web_empty_root"

func TestServerWebLoginIntegration(t *testing.T) {
	if os.Getenv("CLOUDNEST_WEB_LOGIN_TEST") != "1" {
		t.Skip("设置 CLOUDNEST_WEB_LOGIN_TEST=1 后用独占库和本地 Vite 代理验证 Web 登录及目录操作")
	}
	t.Setenv("ACCESS_TOKEN_TTL", "8s")
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	baseURL := prepareServerWebLoginFixture(t, ctx)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	read := func(request *http.Request, status int, result any) []*http.Cookie {
		t.Helper()
		response, err := client.Do(request)
		if err != nil {
			t.Fatal("Vite 代理请求失败", err)
		}
		defer response.Body.Close()
		if response.StatusCode != status {
			t.Fatal("Vite 代理响应状态不符", request.Method, request.URL.Path, response.StatusCode, status)
		}
		if err := json.NewDecoder(response.Body).Decode(result); err != nil {
			t.Fatal("Vite 代理响应格式不符", request.URL.Path, err)
		}
		return response.Cookies()
	}
	get := func(path, accessToken string) *http.Request {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if accessToken != "" {
			request.Header.Set("Authorization", "Bearer "+accessToken)
		}
		return request
	}
	getDirectory := func(accessToken, parentID string, cursor *string) *http.Request {
		query := url.Values{"scope": {"directory"}, "parent_id": {parentID}}
		if cursor != nil {
			query.Set("cursor", *cursor)
		}
		return get("/api/v1/entries?"+query.Encode(), accessToken)
	}
	loginAs := func(username string) (dto.LoginResponse, []*http.Cookie) {
		t.Helper()
		input, err := json.Marshal(dto.LoginRequest{
			Username: username, Password: webLoginFixturePassword,
			InstallationID: "28ee758a-0727-42de-ae7d-3bb33155e148", DeviceName: "隔离测试浏览器",
			ClientType: "web", Platform: "WEB",
		})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/auth/login", bytes.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CloudNest-Client", "web")
		request.Header.Set("Origin", baseURL)
		var response dto.LoginResponse
		cookies := read(request, http.StatusOK, &response)
		return response, cookies
	}
	refreshSession := func(origin string) *http.Request {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/auth/refresh", strings.NewReader(`{"client_type":"web"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CloudNest-Client", "web")
		request.Header.Set("Origin", origin)
		return request
	}

	// 1. 未登录时，本人资料和刷新请求均经正式路由返回 401。
	var anonymous dto.ErrorResponse
	read(get("/api/v1/me", ""), http.StatusUnauthorized, &anonymous)
	if anonymous.Error.Code != "AUTH_REQUIRED" {
		t.Fatal("匿名本人资料请求的错误码不符", anonymous.Error.Code)
	}
	read(refreshSession(baseURL), http.StatusUnauthorized, &anonymous)
	if anonymous.Error.Code != "AUTH_REQUIRED" {
		t.Fatal("缺少刷新 Cookie 的错误码不符", anonymous.Error.Code)
	}

	// 2. 通过现有 Web 登录契约取得 Access Token 和浏览器 Cookie。
	signedIn, cookies := loginAs(webLoginFixtureUsername)
	if signedIn.Data.AccessToken == "" || signedIn.Data.RefreshToken != "" || signedIn.Data.Device.Platform != "WEB" ||
		len(cookies) != 1 || cookies[0].Name != "cloudnest_refresh_token" || cookies[0].Value == "" ||
		!cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/api/v1/auth" {
		t.Fatal("Web 登录的凭证与 Cookie 交付不符")
	}

	// 3. 使用 Access Token 读取真正的本人资料，并核对隔离库中的标识和配额。
	var profile dto.UserProfileResponse
	read(get("/api/v1/me", signedIn.Data.AccessToken), http.StatusOK, &profile)
	want := dto.UserProfileData{
		User:        dto.UserSummary{ID: "7", Username: webLoginFixtureUsername, DisplayName: webLoginFixtureDisplayName},
		RootEntryID: "33", QuotaBytes: 104857600, UsedBytes: 2048, ReservedBytes: 0,
	}
	if profile.Data != want {
		t.Fatal("登录后读回的本人资料不符", profile.Data)
	}
	var firstPage dto.DirectoryPageResponse
	read(getDirectory(signedIn.Data.AccessToken, profile.Data.RootEntryID, nil), http.StatusOK, &firstPage)
	firstPageItems := firstPage.Data.Items
	if len(firstPageItems) != 50 || firstPage.Data.NextCursor == nil ||
		firstPageItems[0].Name != "根目录笔记.md" || firstPageItems[0].EntryType != "FILE" || firstPageItems[0].SizeBytes != 2048 ||
		firstPageItems[1].Name != "根目录文件夹" || firstPageItems[1].EntryType != "DIR" {
		t.Fatal("登录后根目录首页缺少预期的文件、目录或游标")
	}
	// 4. 已登录时仍要求合法 Origin，合法请求仅凭 Cookie 换取新令牌。
	var forbidden dto.ErrorResponse
	read(refreshSession("http://127.0.0.1:1"), http.StatusForbidden, &forbidden)
	var refreshed dto.RefreshResponse
	rotatedCookies := read(refreshSession(baseURL), http.StatusOK, &refreshed)
	if refreshed.Data.AccessToken == "" || refreshed.Data.RefreshToken != "" ||
		len(rotatedCookies) != 1 || rotatedCookies[0].Name != cookies[0].Name ||
		rotatedCookies[0].Value == "" || rotatedCookies[0].Value == cookies[0].Value ||
		!rotatedCookies[0].HttpOnly || rotatedCookies[0].SameSite != http.SameSiteStrictMode ||
		rotatedCookies[0].Path != cookies[0].Path {
		t.Fatal("Web 刷新未按契约轮换 Cookie 或交付访问令牌")
	}
	var restoredProfile dto.UserProfileResponse
	read(get("/api/v1/me", refreshed.Data.AccessToken), http.StatusOK, &restoredProfile)
	if restoredProfile.Data != want {
		t.Fatal("刷新后读回的本人资料不符", restoredProfile.Data)
	}
	t.Log("同源登录和刷新成功，Cookie 已轮换，新令牌读回本人资料，缺 Cookie 返回 401，非白名单来源返回 403")

	// 5. 用恢复后的资料与访问令牌读取同一根目录，核对续页和可见性。
	var restoredPage, nextPage dto.DirectoryPageResponse
	read(getDirectory(refreshed.Data.AccessToken, restoredProfile.Data.RootEntryID, nil), http.StatusOK, &restoredPage)
	if !slices.Equal(restoredPage.Data.Items, firstPageItems) || restoredPage.Data.NextCursor == nil {
		t.Fatal("会话恢复后的根目录首页与登录后的首页不一致")
	}
	read(getDirectory(refreshed.Data.AccessToken, restoredProfile.Data.RootEntryID, restoredPage.Data.NextCursor), http.StatusOK, &nextPage)
	if len(nextPage.Data.Items) != 1 || nextPage.Data.Items[0].Name != "续页目录 01" || nextPage.Data.NextCursor != nil {
		t.Fatal("根目录续页内容或结束游标不符")
	}
	for _, item := range append(firstPageItems, nextPage.Data.Items...) {
		if item.ParentID != profile.Data.RootEntryID || item.Status != "ACTIVE" {
			t.Fatal("根目录返回了不属于该活动列表的条目", item.ID)
		}
	}

	// 6. 以另一合成账号验证真正的空根目录，并核对跨账号读取被拒绝。
	emptyUser, _ := loginAs(webEmptyRootFixtureUsername)
	var emptyProfile dto.UserProfileResponse
	read(get("/api/v1/me", emptyUser.Data.AccessToken), http.StatusOK, &emptyProfile)
	if emptyProfile.Data.RootEntryID != "63" {
		t.Fatal("空目录账号的根目录标识不符")
	}
	var emptyPage dto.DirectoryPageResponse
	read(getDirectory(emptyUser.Data.AccessToken, emptyProfile.Data.RootEntryID, nil), http.StatusOK, &emptyPage)
	if emptyPage.Data.Items == nil || len(emptyPage.Data.Items) != 0 || emptyPage.Data.NextCursor != nil {
		t.Fatal("空根目录应返回空数组和末页游标")
	}
	var denied dto.ErrorResponse
	read(getDirectory(refreshed.Data.AccessToken, emptyProfile.Data.RootEntryID, nil), http.StatusNotFound, &denied)
	if denied.Error.Code != "ENTRY_NOT_FOUND" {
		t.Fatal("跨账号目录查询错误码不符", denied.Error.Code)
	}
	t.Log("根目录登录与会话恢复一致：50 项首页、1 项续页，独立账号根目录为空，跨账号返回 404 ENTRY_NOT_FOUND")

	// 7. 沿文件夹进入两层目录，核对服务端面包屑和空页。
	var subdirectory dto.DirectoryPageResponse
	rootFolder := firstPageItems[1]
	read(getDirectory(refreshed.Data.AccessToken, rootFolder.ID, nil), http.StatusOK, &subdirectory)
	wantSubdirectoryPath := []dto.DirectoryBreadcrumb{
		{ID: restoredProfile.Data.RootEntryID, Name: ""},
		{ID: rootFolder.ID, Name: rootFolder.Name},
	}
	if !slices.Equal(subdirectory.Data.Breadcrumbs, wantSubdirectoryPath) || len(subdirectory.Data.Items) != 1 {
		t.Fatal("子目录的面包屑或条目不符", subdirectory.Data)
	}
	nestedFolder := subdirectory.Data.Items[0]
	if nestedFolder.ParentID != rootFolder.ID || nestedFolder.Name != "子目录中的条目" || nestedFolder.EntryType != "DIR" {
		t.Fatal("子目录未返回预期的下一层入口", nestedFolder)
	}
	var nestedPage dto.DirectoryPageResponse
	read(getDirectory(refreshed.Data.AccessToken, nestedFolder.ID, nil), http.StatusOK, &nestedPage)
	wantNestedPath := append(wantSubdirectoryPath, dto.DirectoryBreadcrumb{ID: nestedFolder.ID, Name: nestedFolder.Name})
	if !slices.Equal(nestedPage.Data.Breadcrumbs, wantNestedPath) ||
		nestedPage.Data.Items == nil || len(nestedPage.Data.Items) != 0 || nestedPage.Data.NextCursor != nil {
		t.Fatal("空子目录应返回完整面包屑和空数组", nestedPage.Data)
	}

	// 8. 按面包屑返回上级，不可见目录仍由现有授权规则拒绝。
	var returned dto.DirectoryPageResponse
	ancestor := nestedPage.Data.Breadcrumbs[1]
	read(getDirectory(refreshed.Data.AccessToken, ancestor.ID, nil), http.StatusOK, &returned)
	if !slices.Equal(returned.Data.Breadcrumbs, wantSubdirectoryPath) ||
		!slices.Equal(returned.Data.Items, subdirectory.Data.Items) {
		t.Fatal("按面包屑返回后的目录不符", returned.Data)
	}
	for _, deniedRequest := range []*http.Request{
		getDirectory(emptyUser.Data.AccessToken, rootFolder.ID, nil),
		getDirectory(refreshed.Data.AccessToken, "56", nil),
	} {
		read(deniedRequest, http.StatusNotFound, &denied)
		if denied.Error.Code != "ENTRY_NOT_FOUND" {
			t.Fatal("子目录不可见时的错误码不符", denied.Error.Code)
		}
	}
	t.Log("子目录联测通过：两层面包屑、空子目录、返回上级、跨账号和已回收目录拒绝")

	// 9. 让真实测试令牌自然到期，使用同一个目录和游标验证刷新后的续页。
	expiringLogin, _ := loginAs(webLoginFixtureUsername)
	select {
	case <-time.After(time.Duration(expiringLogin.Data.ExpiresIn+1) * time.Second):
	case <-ctx.Done():
		t.Fatal("等待隔离访问令牌到期", ctx.Err())
	}
	parentID, cursor := restoredProfile.Data.RootEntryID, restoredPage.Data.NextCursor
	var expired dto.ErrorResponse
	read(getDirectory(expiringLogin.Data.AccessToken, parentID, cursor), http.StatusUnauthorized, &expired)
	if expired.Error.Code != "TOKEN_EXPIRED" {
		t.Fatal("访问令牌到期后的目录错误码不符", expired.Error.Code)
	}
	var renewed dto.RefreshResponse
	read(refreshSession(baseURL), http.StatusOK, &renewed)
	var retriedPage dto.DirectoryPageResponse
	read(getDirectory(renewed.Data.AccessToken, parentID, cursor), http.StatusOK, &retriedPage)
	if !slices.Equal(retriedPage.Data.Items, nextPage.Data.Items) || retriedPage.Data.NextCursor != nil {
		t.Fatal("刷新后的续页与原游标结果不符", retriedPage.Data)
	}

	// 10. 移除客户端 Cookie，确认刷新接口仍按认证失败返回 401。
	client.Jar = nil
	var invalidRefresh dto.ErrorResponse
	read(refreshSession(baseURL), http.StatusUnauthorized, &invalidRefresh)
	if invalidRefresh.Error.Code != "AUTH_REQUIRED" {
		t.Fatal("刷新 Cookie 缺失时的错误码不符", invalidRefresh.Error.Code)
	}
	t.Log("目录续页过期联测通过：401 TOKEN_EXPIRED，刷新 200，原目录和游标重试 200，缺 Cookie 时刷新 401")

	// 11. 在当前合成子目录提交创建及同键同参数请求，再确认列表只有一项新记录。
	createFolder := func(accessToken, parentID, name, key string) *http.Request {
		t.Helper()
		input, err := json.Marshal(map[string]string{"parent_id": parentID, "name": name})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/directories", bytes.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+accessToken)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("Origin", baseURL)
		return request
	}
	const createKey = "fba342b8-ff85-4c1e-a6f2-a0a74b83af05"
	const folderName = "隔离新建目录"
	const submittedName = "  " + folderName + "  "
	var created, repeated dto.CreateDirectoryResponse
	read(createFolder(renewed.Data.AccessToken, rootFolder.ID, submittedName, createKey), http.StatusCreated, &created)
	read(createFolder(renewed.Data.AccessToken, rootFolder.ID, submittedName, createKey), http.StatusCreated, &repeated)
	if repeated.Data != created.Data || repeated.RequestID == created.RequestID || created.Data.Entry.ID == "" ||
		created.Data.Entry.Name != folderName || created.Data.Entry.ParentID != rootFolder.ID || created.Data.Entry.EntryType != "DIR" {
		t.Fatal("同键重试未返回相同的创建快照")
	}

	// 12. 核对重名、非法名称、跨账号父目录及过期会话的正式错误。
	for _, failure := range []struct {
		request *http.Request
		status  int
		code    string
	}{
		{createFolder(renewed.Data.AccessToken, rootFolder.ID, folderName, "web-create-name-conflict"), http.StatusConflict, "NAME_CONFLICT"},
		{createFolder(renewed.Data.AccessToken, rootFolder.ID, "非法/目录", "web-create-invalid-name"), http.StatusBadRequest, "INVALID_NAME"},
		{createFolder(renewed.Data.AccessToken, emptyProfile.Data.RootEntryID, folderName, "web-create-foreign"), http.StatusNotFound, "ENTRY_NOT_FOUND"},
		{createFolder(expiringLogin.Data.AccessToken, rootFolder.ID, "过期请求", "web-create-expired"), http.StatusUnauthorized, "TOKEN_EXPIRED"},
	} {
		var failureResponse dto.ErrorResponse
		read(failure.request, failure.status, &failureResponse)
		if failureResponse.Error.Code != failure.code {
			t.Fatal("创建目录失败响应不符", failureResponse.Error.Code, failure.code)
		}
	}
	var afterCreate dto.DirectoryPageResponse
	read(getDirectory(renewed.Data.AccessToken, rootFolder.ID, nil), http.StatusOK, &afterCreate)
	if len(afterCreate.Data.Items) != 2 || !slices.Contains(afterCreate.Data.Items, created.Data.Entry.DirectoryEntrySummary) {
		t.Fatal("重新读取当前子目录未找到唯一创建结果", afterCreate.Data.Items)
	}
	emptyUser, _ = loginAs(webEmptyRootFixtureUsername)
	read(getDirectory(emptyUser.Data.AccessToken, emptyProfile.Data.RootEntryID, nil), http.StatusOK, &emptyPage)
	if len(emptyPage.Data.Items) != 0 {
		t.Fatal("跨账号拒绝后目标目录应保持为空")
	}
	t.Log("Web 创建目录联测通过：同键两次 201 同一条目，当前目录重读，重名 409、非法名称 400、跨账号 404 和过期 401")

	// 13. 使用目录列表中的 ID 和版本重命名，核对结果和手动同键重试。
	target := firstPageItems[0]
	rename := func(accessToken, revision, name, key string) *http.Request {
		t.Helper()
		body, err := json.Marshal(dto.RenameEntryRequest{ExpectedRevision: revision, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPatch, baseURL+"/api/v1/entries/"+target.ID, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+accessToken)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("Origin", baseURL)
		return request
	}
	const renameKey = "311921fb-1239-4ed6-97e8-60b4aa4bc401"
	const renamedFile = "隔离重命名.md"
	var renamed, renameReplay dto.RenameEntryResponse
	read(rename(renewed.Data.AccessToken, target.Revision, renamedFile, renameKey), http.StatusOK, &renamed)
	read(rename(renewed.Data.AccessToken, target.Revision, renamedFile, renameKey), http.StatusOK, &renameReplay)
	entry := renamed.Data.Entry
	if entry.ID != target.ID || entry.Name != renamedFile || entry.Revision != "2" ||
		renameReplay.Data.Entry.ID != entry.ID || renameReplay.Data.Entry.Revision != entry.Revision ||
		renameReplay.RequestID == renamed.RequestID {
		t.Fatal("重命名成功或同键重试的条目版本不符")
	}

	// 14. 过期版本、重名和跨账号分别走正式拒绝，目录仍读到已保存的名称。
	var staleRename dto.ErrorResponse
	read(rename(renewed.Data.AccessToken, target.Revision, "过期的草稿.md", "web-rename-stale"), http.StatusConflict, &staleRename)
	if staleRename.Error.Code != "REVISION_CONFLICT" || staleRename.Error.Details["current_revision"] != entry.Revision {
		t.Fatal("重命名版本冲突响应不符", staleRename.Error)
	}
	var duplicateName dto.ErrorResponse
	read(rename(renewed.Data.AccessToken, entry.Revision, rootFolder.Name, "web-rename-duplicate"), http.StatusConflict, &duplicateName)
	if duplicateName.Error.Code != "NAME_CONFLICT" {
		t.Fatal("重命名的同名冲突响应不符", duplicateName.Error)
	}
	var foreignRename dto.ErrorResponse
	read(rename(emptyUser.Data.AccessToken, entry.Revision, "越权草稿.md", "web-rename-foreign"), http.StatusNotFound, &foreignRename)
	if foreignRename.Error.Code != "ENTRY_NOT_FOUND" {
		t.Fatal("其他账号的重命名响应不符", foreignRename.Error)
	}
	var afterRename dto.DirectoryPageResponse
	read(getDirectory(renewed.Data.AccessToken, parentID, nil), http.StatusOK, &afterRename)
	if len(afterRename.Data.Items) != len(firstPageItems) ||
		!slices.ContainsFunc(afterRename.Data.Items, func(item dto.DirectoryEntrySummary) bool {
			return item.ID == target.ID && item.Name == renamedFile && item.Revision == entry.Revision
		}) {
		t.Fatal("重读目录未找到已更新的文件名称与版本")
	}
	t.Log("Web 重命名联测通过：同一 ID 更新到版本 2，原键重试结果一致，重名及过期版本返回 409，跨账号返回 404")

	// 15. 同名目标拒绝移动，随后用原版本移到另一层目录。
	move := func(targetParentID, revision, key string) *http.Request {
		t.Helper()
		body, err := json.Marshal(dto.MoveEntryRequest{TargetParentID: targetParentID, ExpectedRevision: revision})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/entries/"+entry.ID+"/move", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+renewed.Data.AccessToken)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("Origin", baseURL)
		return request
	}
	var collision dto.CreateDirectoryResponse
	read(createFolder(renewed.Data.AccessToken, rootFolder.ID, renamedFile, "web-move-collision-fixture"), http.StatusCreated, &collision)
	var moveConflict dto.ErrorResponse
	read(move(rootFolder.ID, entry.Revision, "web-move-name-conflict"), http.StatusConflict, &moveConflict)
	if moveConflict.Error.Code != "NAME_CONFLICT" {
		t.Fatal("移动的同名冲突响应不符", moveConflict.Error)
	}

	const moveKey = "986be2cc-dbb9-467e-922d-1867660e689f"
	var moved, moveReplay dto.MoveEntryResponse
	read(move(nestedFolder.ID, entry.Revision, moveKey), http.StatusOK, &moved)
	read(move(nestedFolder.ID, entry.Revision, moveKey), http.StatusOK, &moveReplay)
	movedEntry := moved.Data.Entry
	if movedEntry.ID != entry.ID || movedEntry.ParentID == nil || *movedEntry.ParentID != nestedFolder.ID ||
		movedEntry.Name != entry.Name || movedEntry.Revision != "3" ||
		moveReplay.Data.Entry.ID != entry.ID || moveReplay.Data.Entry.Revision != movedEntry.Revision {
		t.Fatal("移动成功或同键重试未返回预期条目")
	}

	// 16. 确认旧版本和他人目标仍被拒绝，重读两端核对唯一位置。
	var staleMove dto.ErrorResponse
	read(move(parentID, entry.Revision, "web-move-stale"), http.StatusConflict, &staleMove)
	if staleMove.Error.Code != "REVISION_CONFLICT" || staleMove.Error.Details["current_revision"] != movedEntry.Revision {
		t.Fatal("移动版本冲突响应不符", staleMove.Error)
	}
	var foreignMove dto.ErrorResponse
	read(move(emptyProfile.Data.RootEntryID, movedEntry.Revision, "web-move-foreign"), http.StatusNotFound, &foreignMove)
	if foreignMove.Error.Code != "ENTRY_NOT_FOUND" {
		t.Fatal("移动到其他账号目录的响应不符", foreignMove.Error)
	}
	var sourceAfterMove, targetAfterMove dto.DirectoryPageResponse
	read(getDirectory(renewed.Data.AccessToken, parentID, nil), http.StatusOK, &sourceAfterMove)
	read(getDirectory(renewed.Data.AccessToken, nestedFolder.ID, nil), http.StatusOK, &targetAfterMove)
	if slices.ContainsFunc(sourceAfterMove.Data.Items, func(item dto.DirectoryEntrySummary) bool { return item.ID == entry.ID }) ||
		len(targetAfterMove.Data.Items) != 1 || targetAfterMove.Data.Items[0].ID != entry.ID ||
		targetAfterMove.Data.Items[0].Revision != movedEntry.Revision {
		t.Fatal("移动后重读源与目标目录的结果不符")
	}
	t.Log("Web 单项移动联测通过：同名和过期版本返回 409，跨账号返回 404，移动后仅目标有原 ID 与新版本")

	// 17. 提交过期版本和其他账号的回收请求，确认文件仍在原位置。
	trash := func(accessToken, entryID, revision, key string) *http.Request {
		t.Helper()
		body, err := json.Marshal(dto.TrashEntryRequest{ExpectedRevision: &revision})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/entries/"+entryID+"/trash", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+accessToken)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("Origin", baseURL)
		return request
	}
	var staleTrash, foreignTrash dto.ErrorResponse
	read(trash(renewed.Data.AccessToken, movedEntry.ID, entry.Revision, "web-trash-stale"), http.StatusConflict, &staleTrash)
	if staleTrash.Error.Code != "REVISION_CONFLICT" || staleTrash.Error.Details["current_revision"] != movedEntry.Revision {
		t.Fatal("回收版本冲突响应不符", staleTrash.Error)
	}
	read(trash(emptyUser.Data.AccessToken, movedEntry.ID, movedEntry.Revision, "web-trash-foreign"), http.StatusNotFound, &foreignTrash)
	if foreignTrash.Error.Code != "ENTRY_NOT_FOUND" {
		t.Fatal("回收其他账号条目的响应不符", foreignTrash.Error)
	}
	var beforeTrash dto.DirectoryPageResponse
	read(getDirectory(renewed.Data.AccessToken, nestedFolder.ID, nil), http.StatusOK, &beforeTrash)
	if !slices.Equal(beforeTrash.Data.Items, targetAfterMove.Data.Items) {
		t.Fatal("被拒绝的回收请求改变了原条目")
	}

	// 18. 用当前版本回收，核对原目录、回收站和已用空间。
	const trashKey = "77097013-ddbb-44f3-a82e-9b68aa63239a"
	var trashed, trashReplay dto.TrashEntryResponse
	read(trash(renewed.Data.AccessToken, movedEntry.ID, movedEntry.Revision, trashKey), http.StatusOK, &trashed)
	read(trash(renewed.Data.AccessToken, movedEntry.ID, movedEntry.Revision, trashKey), http.StatusOK, &trashReplay)
	trashSnapshot := trashed.Data.Entry
	if trashSnapshot.ID != movedEntry.ID || trashSnapshot.ParentID != nestedFolder.ID ||
		trashSnapshot.Status != "TRASHED" || trashSnapshot.Revision != "4" || trashSnapshot.DeletedAt.IsZero() ||
		trashReplay.Data != trashed.Data || trashReplay.RequestID == trashed.RequestID {
		t.Fatal("回收或同键重试未返回预期的软删除快照")
	}
	var afterTrash dto.DirectoryPageResponse
	var trashPage dto.TrashPageResponse
	var profileAfterTrash dto.UserProfileResponse
	read(getDirectory(renewed.Data.AccessToken, nestedFolder.ID, nil), http.StatusOK, &afterTrash)
	read(get("/api/v1/trash", renewed.Data.AccessToken), http.StatusOK, &trashPage)
	read(get("/api/v1/me", renewed.Data.AccessToken), http.StatusOK, &profileAfterTrash)
	if len(afterTrash.Data.Items) != 0 ||
		!slices.ContainsFunc(trashPage.Data.Items, func(item dto.TrashListItem) bool {
			return item.TrashedEntrySummary == trashSnapshot && item.SizeBytes == target.SizeBytes
		}) || profileAfterTrash.Data.UsedBytes != restoredProfile.Data.UsedBytes {
		t.Fatal("回收后的目录、回收站或已用空间不符")
	}
	t.Log("Web 单项回收联测通过：旧版本 409 不改动，跨账号 404，成功后同一条目出现在回收站且已用空间不变")

	// 19. 从正式接口读取含原父路径的第一页，原样传回游标续读。
	wantOriginalPath := []dto.DirectoryBreadcrumb{
		{ID: parentID, Name: ""},
		{ID: rootFolder.ID, Name: rootFolder.Name},
		{ID: nestedFolder.ID, Name: nestedFolder.Name},
	}
	if len(trashPage.Data.Items) != 50 || trashPage.Data.NextCursor == nil ||
		trashPage.Data.Items[0].TrashedEntrySummary != trashSnapshot ||
		!slices.Equal(trashPage.Data.Items[0].OriginalPath, wantOriginalPath) {
		t.Fatal("回收站首页未返回预期原位置或游标")
	}
	continuation := "/api/v1/trash?" + url.Values{"cursor": {*trashPage.Data.NextCursor}}.Encode()
	var trashContinuation dto.TrashPageResponse
	read(get(continuation, renewed.Data.AccessToken), http.StatusOK, &trashContinuation)
	if len(trashContinuation.Data.Items) != 2 || trashContinuation.Data.NextCursor != nil ||
		trashContinuation.Data.Items[1].Name != "隔离回收续页 01" {
		t.Fatal("回收站续页条目或末页游标不符")
	}

	// 20. 空回收站来自第二账号，同一游标跨账号和未授权读取都被拒绝。
	var emptyTrash dto.TrashPageResponse
	read(get("/api/v1/trash", emptyUser.Data.AccessToken), http.StatusOK, &emptyTrash)
	if emptyTrash.Data.Items == nil || len(emptyTrash.Data.Items) != 0 || emptyTrash.Data.NextCursor != nil {
		t.Fatal("空回收站应返回空数组和末页游标")
	}
	var foreignCursor, anonymousTrash dto.ErrorResponse
	read(get(continuation, emptyUser.Data.AccessToken), http.StatusBadRequest, &foreignCursor)
	read(get("/api/v1/trash", ""), http.StatusUnauthorized, &anonymousTrash)
	if foreignCursor.Error.Code != "INVALID_CURSOR" || anonymousTrash.Error.Code != "AUTH_REQUIRED" {
		t.Fatal("回收站游标绑定或认证拒绝响应不符")
	}
	t.Log("Web 回收站列表联测通过：含原位置的 50 项首页、2 项续页、独立账号空列表，跨账号游标及匿名请求被拒")

	// 21. 拒绝过期版本和其他账号；用当前版本恢复文件至原父目录。
	restore := func(accessToken, entryID, revision, key string) *http.Request {
		t.Helper()
		body, err := json.Marshal(dto.RestoreEntryRequest{ExpectedRevision: &revision})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/trash/"+entryID+"/restore", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+accessToken)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("Origin", baseURL)
		return request
	}
	var staleRestore, foreignRestore dto.ErrorResponse
	read(restore(renewed.Data.AccessToken, trashSnapshot.ID, movedEntry.Revision, "web-restore-stale"), http.StatusConflict, &staleRestore)
	read(restore(emptyUser.Data.AccessToken, trashSnapshot.ID, trashSnapshot.Revision, "web-restore-foreign"), http.StatusNotFound, &foreignRestore)
	if staleRestore.Error.Code != "REVISION_CONFLICT" || staleRestore.Error.Details["current_revision"] != trashSnapshot.Revision ||
		foreignRestore.Error.Code != "ENTRY_NOT_FOUND" {
		t.Fatal("恢复时的认证或版本边界不符")
	}
	const restoreKey = "623dbeda-412a-4f3a-9e1a-fbe6e4d4c875"
	var restored, restoreReplay dto.RestoreEntryResponse
	read(restore(renewed.Data.AccessToken, trashSnapshot.ID, trashSnapshot.Revision, restoreKey), http.StatusOK, &restored)
	read(restore(renewed.Data.AccessToken, trashSnapshot.ID, trashSnapshot.Revision, restoreKey), http.StatusOK, &restoreReplay)
	restoredFile := restored.Data.Entry
	if restoredFile.ID != trashSnapshot.ID || restoredFile.ParentID != nestedFolder.ID ||
		restoredFile.Name != trashSnapshot.Name || restoredFile.Status != "ACTIVE" || restoredFile.Revision != "5" ||
		restoreReplay.Data != restored.Data || restoreReplay.RequestID == restored.RequestID {
		t.Fatal("原位恢复或同键重试的结果不符")
	}
	var originalAfterRestore dto.DirectoryPageResponse
	read(getDirectory(renewed.Data.AccessToken, nestedFolder.ID, nil), http.StatusOK, &originalAfterRestore)
	if !slices.ContainsFunc(originalAfterRestore.Data.Items, func(item dto.DirectoryEntrySummary) bool {
		return item.ID == restoredFile.ID && item.Name == restoredFile.Name && item.Revision == restoredFile.Revision
	}) {
		t.Fatal("原目录中没有已恢复文件")
	}

	// 22. 原父目录在列表读取后被回收，并在根目录准备同名项。
	fallbackIndex := slices.IndexFunc(trashPage.Data.Items, func(item dto.TrashListItem) bool { return item.ID == "250" })
	if fallbackIndex < 0 {
		t.Fatal("回收站未提供用于回退验证的原版本")
	}
	fallback := trashPage.Data.Items[fallbackIndex]
	var rootCollision dto.CreateDirectoryResponse
	var trashedParent dto.TrashEntryResponse
	read(createFolder(renewed.Data.AccessToken, parentID, fallback.Name, "web-restore-root-collision"), http.StatusCreated, &rootCollision)
	read(trash(renewed.Data.AccessToken, rootFolder.ID, rootFolder.Revision, "web-restore-hide-parent"), http.StatusOK, &trashedParent)
	var restoredAtRoot dto.RestoreEntryResponse
	read(restore(renewed.Data.AccessToken, fallback.ID, fallback.Revision, "web-restore-fallback"), http.StatusOK, &restoredAtRoot)
	fallbackResult := restoredAtRoot.Data.Entry
	if fallbackResult.ID != fallback.ID || fallbackResult.ParentID != parentID ||
		fallbackResult.Name != fallback.Name+" (1)" || fallbackResult.Status != "ACTIVE" {
		t.Fatal("恢复至根目录时的实际位置或编号名称不符")
	}
	var rootAfterRestore dto.DirectoryPageResponse
	var trashAfterRestore dto.TrashPageResponse
	read(getDirectory(renewed.Data.AccessToken, parentID, nil), http.StatusOK, &rootAfterRestore)
	read(get("/api/v1/trash", renewed.Data.AccessToken), http.StatusOK, &trashAfterRestore)
	if !slices.ContainsFunc(rootAfterRestore.Data.Items, func(item dto.DirectoryEntrySummary) bool {
		return item.ID == fallbackResult.ID && item.Name == fallbackResult.Name
	}) || !slices.ContainsFunc(rootAfterRestore.Data.Items, func(item dto.DirectoryEntrySummary) bool {
		return item.ID == rootCollision.Data.Entry.ID && item.Name == fallback.Name
	}) || slices.ContainsFunc(trashAfterRestore.Data.Items, func(item dto.TrashListItem) bool {
		return item.ID == restoredFile.ID || item.ID == fallbackResult.ID
	}) {
		t.Fatal("恢复后目录与回收站读取结果不一致")
	}
	t.Log("Web 单项恢复联测通过：原位恢复、根目录回退与编号、同键结果、过期版本 409 和跨账号 404")
}

func prepareServerWebLoginFixture(t *testing.T, ctx context.Context) string {
	t.Helper()
	// 1. 创建独占库及两个合成账号，存储依赖仅走本地只读桩。
	pool, databaseName := prepareServerIntegrationDatabase(t, ctx)
	prepareServerReadOnlyStorageStub(t, "fixture-web-login")
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(webLoginFixturePassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		query string
		args  []any
	}{
		{"INSERT INTO users(id,username,display_name,password_hash,storage_quota,used_storage) VALUES(7,?,?,?,104857600,2048),(8,?,?,?,104857600,0)",
			[]any{webLoginFixtureUsername, webLoginFixtureDisplayName, string(passwordHash), webEmptyRootFixtureUsername, "隔离空目录用户", string(passwordHash)}},
		{"INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(33,7,'',X'','DIR'),(63,8,'',X'','DIR')", nil},
		{"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,status,verified_sha256,verified_at) VALUES(41,7,'MINIO','fixture-web-login','synthetic-root-note',2048,'READY',UNHEX(REPEAT('01',32)),UTC_TIMESTAMP(6))", nil},
		{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,modified_at) VALUES(53,7,33,?,?,'FILE','DOCUMENT',41,2048,'2026-01-03 00:00:00')",
			[]any{"根目录笔记.md", []byte("根目录笔记.md")}},
		{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,modified_at) VALUES(54,7,33,?,?,'DIR','2026-01-02 00:00:00')",
			[]any{"根目录文件夹", []byte("根目录文件夹")}},
		{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(55,7,54,?,?,'DIR')",
			[]any{"子目录中的条目", []byte("子目录中的条目")}},
		{"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(56,7,33,?,?,'DIR','TRASHED',UTC_TIMESTAMP(6))",
			[]any{"回收站中的条目", []byte("回收站中的条目")}},
	}
	for _, statement := range statements {
		if _, err := pool.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	for number := 1; number <= 49; number++ {
		name := fmt.Sprintf("续页目录 %02d", number)
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,modified_at) VALUES(?,7,33,?,?,'DIR','2026-01-01 00:00:00')",
			100+number, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	for number := 1; number <= 50; number++ {
		name := fmt.Sprintf("隔离回收续页 %02d", number)
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(?,7,54,?,?,'DIR','TRASHED',UTC_TIMESTAMP(6)-INTERVAL 1 DAY)",
			200+number, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
	}

	// 2. 给真实后端和 Vite 分配本机端口，Origin 白名单指向该 Vite 实例。
	apiAddress, webAddress := reserveWebLoginAddress(t), reserveWebLoginAddress(t)
	baseURL := "http://" + webAddress
	t.Setenv("CLOUDNEST_HTTP_ADDR", apiAddress)
	t.Setenv("CLOUDNEST_DB_NAME", databaseName)
	t.Setenv("CLOUDNEST_TEMP_DIR", t.TempDir())
	t.Setenv("CLOUDNEST_JWT_SECRET", base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte("w"), 32)))
	t.Setenv("CLOUDNEST_COOKIE_SECURE", "false")
	t.Setenv("CLOUDNEST_ALLOWED_ORIGINS", baseURL)
	t.Setenv("VITE_CLOUDNEST_DATA_MODE", "api")
	t.Setenv("CLOUDNEST_WEB_API_TARGET", "http://"+apiAddress)
	apiContext, stopAPI := context.WithCancel(ctx)
	apiDone := make(chan error, 1)
	go func() { apiDone <- run(apiContext, logging.New(io.Discard)) }()
	t.Cleanup(func() {
		stopAPI()
		select {
		case err := <-apiDone:
			if err != nil {
				t.Error("隔离后端退出失败", err)
			}
		case <-time.After(35 * time.Second):
			t.Error("隔离后端未在期限内停止")
		}
	})
	waitForServerIntegrationHTTP(t, ctx, apiAddress)
	_, port, err := net.SplitHostPort(webAddress)
	if err != nil {
		t.Fatal(err)
	}
	viteContext, stopVite := context.WithCancel(ctx)
	vite := exec.CommandContext(viteContext, "node", "node_modules/vite/bin/vite.js", "--host", "127.0.0.1", "--port", port, "--strictPort")
	vite.Dir = filepath.Join("..", "..", "apps", "web")
	var viteLogs bytes.Buffer
	vite.Stdout, vite.Stderr = &viteLogs, &viteLogs
	if err := vite.Start(); err != nil {
		stopVite()
		t.Fatal("启动隔离 Vite 失败", err)
	}
	viteDone := make(chan error, 1)
	go func() { viteDone <- vite.Wait() }()
	t.Cleanup(func() {
		stopVite()
		select {
		case <-viteDone:
		case <-time.After(5 * time.Second):
			t.Error("隔离 Vite 未在期限内退出")
		}
	})

	// 3. 等待 Vite 页面可访问，再交给联测。
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	for {
		response, err := client.Get(baseURL + "/")
		if err == nil {
			page, readError := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readError == nil && response.StatusCode == http.StatusOK && strings.Contains(string(page), `id="root"`) {
				return baseURL
			}
		}
		select {
		case err := <-viteDone:
			viteDone <- err
			t.Fatal("隔离 Vite 提前退出", err, viteLogs.String())
		case <-ctx.Done():
			t.Fatal("隔离 Vite 未在期限内就绪")
		case <-ticker.C:
		}
	}
}

func reserveWebLoginAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}
