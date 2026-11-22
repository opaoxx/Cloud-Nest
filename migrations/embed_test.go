package migrations

import "testing"

func TestOrderedInitialMigration(t *testing.T) {
	// 1. 核对首期清单引用的版本、文件、SQL 和已应用摘要。
	const initialChecksum = "627a990d6b3e5cf77b3e01ff468806cfbb77da42a96bb46f0162cfedc0a4cfc7"
	catalog := Ordered()
	if len(catalog) < 3 {
		t.Fatal("迁移清单缺少版本 1、2 或 3")
	}
	initial := catalog[0]
	checksum := initial.Checksum()
	if initial.Version != 1 || initial.Name != "000001_initial.up.sql" ||
		initial.SQL != InitialSQL || checksum != initialChecksum {
		t.Fatalf("初始迁移的版本、文件或摘要有误，实际为 %d %s %s", initial.Version, initial.Name, checksum)
	}
	const partsChecksum = "86004bbb389c84e7713cae3b1393c8318148711edfa6e64b52f7d2435a132391"
	parts := catalog[1]
	if parts.Version != 2 || parts.Name != "000002_upload_parts.up.sql" || parts.Checksum() != partsChecksum {
		t.Fatalf("分片迁移的版本、文件或摘要有误，实际为 %d %s %s", parts.Version, parts.Name, parts.Checksum())
	}
	const syncChecksum = "4ba46ef19b0ed5c206da41121634250a23cd46e6ac239762d182fc4a7d9f2c0d"
	syncLog := catalog[2]
	if syncLog.Version != 3 || syncLog.Name != "000003_sync_log.up.sql" || syncLog.Checksum() != syncChecksum {
		t.Fatalf("同步迁移的版本、文件或摘要有误，实际为 %d %s %s", syncLog.Version, syncLog.Name, syncLog.Checksum())
	}

	// 2. 确认版本严格递增，修改本次返回的切片后仍可读到原始定义。
	for index := 1; index < len(catalog); index++ {
		if catalog[index-1].Version >= catalog[index].Version {
			t.Fatal("内嵌迁移的版本须严格递增")
		}
	}
	catalog[0] = Migration{}
	if reloaded := Ordered(); len(reloaded) != len(catalog) || reloaded[0] != initial {
		t.Fatal("调用方的修改影响了再次读取的迁移清单")
	}
}
