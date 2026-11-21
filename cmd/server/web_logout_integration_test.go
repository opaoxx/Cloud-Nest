package main

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestServerWebLogoutIntegration(t *testing.T) {
	cli := webFixtureBrowserCLI(t, "CLOUDNEST_WEB_AUTH_PLAYWRIGHT_CLI")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	// 1. 使用独占 MySQL、合成账号和只读存储桩，准备一个可进入的文件夹。
	startIsolatedWebMySQL(t, ctx)
	pool, webURL := prepareWebPurgePreviewFixture(t, ctx)
	if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type)
		VALUES(71,7,33,'隔离退出目录',CAST('隔离退出目录' AS BINARY),'DIR')`); err != nil {
		t.Fatal(err)
	}
	before := readWebPurgePreviewSnapshot(t, ctx, pool)

	// 2. 检查浏览器退出、刷新交付顺序、服务拒绝旧凭证和独立会话。
	runWebBrowserProbe(t, ctx, cli, webURL, "web_logout.js", "CN534 browser checks passed")
	t.Log("Edge 验收通过：失败保留登录，连点只提交一次，真实退出等待在途刷新，清理 Cookie，重载不能恢复已撤销会话")

	// 3. 确认只撤销当前会话，不改变同设备对照会话或文件业务记录。
	var sessions, devices, revoked, rotatedAndRevoked int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(DISTINCT device_id),COUNT(revoked_at),
		COUNT(CASE WHEN token_generation=2 AND revoked_at IS NOT NULL THEN 1 END)
		FROM auth_sessions WHERE user_id=7`).Scan(&sessions, &devices, &revoked, &rotatedAndRevoked); err != nil {
		t.Fatal(err)
	}
	if sessions != 2 || devices != 1 || revoked != 1 || rotatedAndRevoked != 1 {
		t.Fatalf("会话状态不符，记录=%d 设备=%d 已撤销=%d 轮换后撤销=%d", sessions, devices, revoked, rotatedAndRevoked)
	}
	if !slices.Equal(before, readWebPurgePreviewSnapshot(t, ctx, pool)) {
		t.Fatal("退出改变了隔离文件业务快照")
	}
	t.Log("MySQL 核对通过：同设备的两条会话仅撤销当前一条，目录、对象、配额和幂等记录保持原样")
}
