package domain_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestStoredObjectStatusValues(t *testing.T) {
	for _, fixture := range []struct {
		status domain.StoredObjectStatus
		text   string
	}{
		{domain.StoredObjectStatusStaging, "STAGING"},
		{domain.StoredObjectStatusVerifying, "VERIFYING"},
		{domain.StoredObjectStatusReady, "READY"},
		{domain.StoredObjectStatusOrphaned, "ORPHANED"},
		{domain.StoredObjectStatusDeleting, "DELETING"},
		{domain.StoredObjectStatusDeleted, "DELETED"},
	} {
		t.Run(fixture.text, func(t *testing.T) {
			// 1. 检查常量与数据库文本转换后的状态。
			status := domain.StoredObjectStatus(fixture.text)
			if string(fixture.status) != fixture.text || !fixture.status.IsValid() {
				t.Fatal("存储对象状态常量或精确匹配错误")
			}
			// 2. 重复判断时保留原始状态文本。
			for range 3 {
				if !status.IsValid() || string(status) != fixture.text {
					t.Fatal("存储对象状态判断或原值保护错误")
				}
			}
		})
	}
}

func TestStoredObjectStatusInvalidValues(t *testing.T) {
	// 1. 检查零值、其他业务状态、错误拼写及非法编码。
	invalidValues := []string{
		"", " ", "ACTIVE", "CREATING", "UPLOADING", "COMPLETED", "FAILED",
		"STAGED", "ORPHAN", "DELETE", "READY\x00", "DELETED\n", "已就绪",
		"ＲＥＡＤＹ", string([]byte{0xff}),
	}
	// 2. 为六种合法状态构造大小写、空白和后缀变体。
	for _, text := range []string{"STAGING", "VERIFYING", "READY", "ORPHANED", "DELETING", "DELETED"} {
		invalidValues = append(invalidValues, strings.ToLower(text), " "+text, text+" ", text+"_UNKNOWN")
	}
	for _, text := range invalidValues {
		t.Run(text, func(t *testing.T) {
			status := domain.StoredObjectStatus(text)
			if status.IsValid() || string(status) != text {
				t.Fatal("非法存储对象状态被接受或被改写")
			}
		})
	}
}

func TestStoredObjectStatusMigrationConstraint(t *testing.T) {
	// 1. 从正式迁移的对象表中读取状态约束与默认值。
	tableMatch := regexp.MustCompile(`(?s)CREATE TABLE stored_objects \((.*?)\) ENGINE=InnoDB`).FindStringSubmatch(migrations.InitialSQL)
	if len(tableMatch) != 2 {
		t.Fatal("正式迁移缺少存储对象表")
	}
	statusMatch := regexp.MustCompile(`CONSTRAINT ck_objects_status CHECK \(status IN \(([^)]+)\)\)`).FindStringSubmatch(tableMatch[1])
	defaultMatch := regexp.MustCompile(`(?m)^ status [^\n]+ DEFAULT '([^']+)'`).FindStringSubmatch(tableMatch[1])
	if len(statusMatch) != 2 || len(defaultMatch) != 2 {
		t.Fatal("正式迁移缺少对象状态约束或默认值")
	}
	statuses := strings.Split(statusMatch[1], ",")
	if len(statuses) != 6 || defaultMatch[1] != string(domain.StoredObjectStatusStaging) {
		t.Fatal("正式迁移的对象状态数量或暂存默认值发生变化")
	}
	// 2. 逐项核对合法值，防止领域模型与正式表约束出现差异。
	seen := make(map[string]bool, len(statuses))
	for _, text := range statuses {
		text = strings.Trim(text, " '")
		t.Run(text, func(t *testing.T) {
			if seen[text] || !domain.StoredObjectStatus(text).IsValid() {
				t.Fatal("存储对象领域状态与正式迁移约束不一致")
			}
			seen[text] = true
		})
	}
}
