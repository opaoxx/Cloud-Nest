package domain_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestUploadStatusValues(t *testing.T) {
	for _, fixture := range []struct {
		status   domain.UploadStatus
		text     string
		terminal bool
	}{
		{domain.UploadStatusCreating, "CREATING", false},
		{domain.UploadStatusUploading, "UPLOADING", false},
		{domain.UploadStatusCompleting, "COMPLETING", false},
		{domain.UploadStatusVerifying, "VERIFYING", false},
		{domain.UploadStatusCompleted, "COMPLETED", true},
		{domain.UploadStatusCancelling, "CANCELLING", false},
		{domain.UploadStatusCancelled, "CANCELLED", true},
		{domain.UploadStatusExpiring, "EXPIRING", false},
		{domain.UploadStatusExpired, "EXPIRED", true},
		{domain.UploadStatusFailed, "FAILED", true},
	} {
		t.Run(fixture.text, func(t *testing.T) {
			// 1. 检查每个常量的精确文本，以及数据库值转换后的合法性。
			status := domain.UploadStatus(fixture.text)
			if string(fixture.status) != fixture.text || !status.IsValid() {
				t.Fatal("上传状态常量或精确匹配错误")
			}
			// 2. 区分处理中与已终结，重复读取保持原始状态。
			for range 3 {
				if status.IsTerminal() != fixture.terminal || string(status) != fixture.text {
					t.Fatal("上传终态判断或原值保护错误")
				}
			}
		})
	}
}

func TestUploadStatusInvalidValues(t *testing.T) {
	// 1. 检查零值、其他业务状态、错误拼写及非法编码。
	invalidValues := []string{
		"", " ", "ACTIVE", "READY", "STAGING", "PAUSED", "CANCELED", "CANCELING",
		"COMPLETED\x00", "FAILED\n", "已完成", "ＣＯＭＰＬＥＴＥＤ", string([]byte{0xff}),
	}
	// 2. 为所有合法状态构造大小写、前后空白及后缀变体。
	for _, text := range []string{
		"CREATING", "UPLOADING", "COMPLETING", "VERIFYING", "COMPLETED",
		"CANCELLING", "CANCELLED", "EXPIRING", "EXPIRED", "FAILED",
	} {
		invalidValues = append(invalidValues, strings.ToLower(text), " "+text, text+" ", text+"_UNKNOWN")
	}
	for _, text := range invalidValues {
		t.Run(text, func(t *testing.T) {
			status := domain.UploadStatus(text)
			if status.IsValid() || status.IsTerminal() || string(status) != text {
				t.Fatal("非法上传状态被接受、被归入终态或被改写")
			}
		})
	}
}

func TestUploadStatusMigrationConstraints(t *testing.T) {
	// 1. 从已嵌入的正式迁移中读取合法状态和终态约束。
	statusConstraint := regexp.MustCompile(`CONSTRAINT ck_uploads_status CHECK \(status IN \(([^)]+)\)\)`)
	terminalConstraint := regexp.MustCompile(`CONSTRAINT ck_uploads_terminal CHECK \(status NOT IN \(([^)]+)\) OR reserved_bytes=0\)`)
	statusMatch := statusConstraint.FindStringSubmatch(migrations.InitialSQL)
	terminalMatch := terminalConstraint.FindStringSubmatch(migrations.InitialSQL)
	if len(statusMatch) != 2 || len(terminalMatch) != 2 {
		t.Fatal("正式迁移缺少上传状态或终态配额约束")
	}
	statuses := strings.Split(statusMatch[1], ",")
	terminalTexts := strings.Split(terminalMatch[1], ",")
	if len(statuses) != 10 || len(terminalTexts) != 4 {
		t.Fatal("正式迁移的上传状态或终态数量发生变化")
	}
	terminals := make(map[string]bool, len(terminalTexts))
	for _, text := range terminalTexts {
		terminals[strings.Trim(text, " '")] = true
	}
	// 2. 逐项核对领域判断，确认处理中的取消/过期仍属于非终态。
	seen := make(map[string]bool, len(statuses))
	for _, text := range statuses {
		text = strings.Trim(text, " '")
		t.Run(text, func(t *testing.T) {
			status := domain.UploadStatus(text)
			if seen[text] || !status.IsValid() || status.IsTerminal() != terminals[text] {
				t.Fatal("领域状态与正式迁移的合法值或终态约束不一致")
			}
			seen[text] = true
		})
	}
	for terminal := range terminals {
		if !seen[terminal] {
			t.Fatal("迁移终态未列入合法上传状态")
		}
	}
}
