package service

import (
	"crypto/sha256"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestValidateSyncChangePage(t *testing.T) {
	// 1. 用现有写入方法准备连续的四条事件，交错放入两种载荷。
	parentID := uint64(11)
	directory := domain.FileEntry{ID: 12, ParentID: &parentID, Name: "资料", Revision: 1}
	file := domain.FileEntry{ID: 13, ParentID: &parentID, Name: "记录.md", Revision: 1, EntryType: "FILE", Status: "ACTIVE"}
	digest := sha256.Sum256(nil)
	events := make([]domain.SyncChangeEvent, 0, 4)
	addEvent := func(written domain.ChangeEventInput, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, domain.SyncChangeEvent{
			Sequence: 18 + uint64(len(events)), EntryID: written.EntryID, Revision: written.Revision,
			EventType: written.EventType, Payload: written.Payload,
		})
	}
	addEvent(domain.NewEntryChangeEvent("DIRECTORY_CREATED", &directory))
	addEvent(domain.NewFilePublishedChangeEvent(&file, digest))
	directory.Name, directory.Revision = "改名后的目录", 2
	addEvent(domain.NewEntryChangeEvent("ENTRY_RENAMED", &directory))
	file.Revision = 2
	addEvent(domain.NewFileContentReplacedChangeEvent(&file, domain.UploadContentDigest{SHA256: digest}))

	// 2. 每次只制造一种异常，验证整页拒绝和对应的错误位置。
	for _, scenario := range []struct {
		name                string
		position, highWater uint64
		pageSize            int
		prepare             func([]domain.SyncChangeEvent) []domain.SyncChangeEvent
		errorText           string
	}{
		{name: "mixed event page", position: 17, highWater: 21, pageSize: 4},
		{name: "valid limited page before high water", position: 17, highWater: 21, pageSize: 2,
			prepare: func(page []domain.SyncChangeEvent) []domain.SyncChangeEvent { return page[:2] }},
		{name: "empty page at high water", position: 21, highWater: 21, pageSize: 4,
			prepare: func(page []domain.SyncChangeEvent) []domain.SyncChangeEvent { return page[:0] }},
		{name: "missing event stops before payload checks", position: 17, highWater: 21, pageSize: 4,
			prepare: func(page []domain.SyncChangeEvent) []domain.SyncChangeEvent {
				page[0].Payload = nil
				return page[:3]
			}, errorText: "应读 4 条"},
		{name: "sequence gap stops before payload checks", position: 17, highWater: 22, pageSize: 4,
			prepare: func(page []domain.SyncChangeEvent) []domain.SyncChangeEvent {
				page[0].Payload = nil
				page[2].Sequence++
				page[3].Sequence++
				return page
			}, errorText: "预期序号 20"},
		{name: "corrupt published digest in middle", position: 17, highWater: 21, pageSize: 4,
			prepare: func(page []domain.SyncChangeEvent) []domain.SyncChangeEvent {
				page[1].Payload = []byte(strings.ReplaceAll(string(page[1].Payload), `"sha256":"`, `"sha256":"g`))
				return page
			}, errorText: "序号 19 的增量事件失败"},
		{name: "metadata ID differs in middle", position: 17, highWater: 21, pageSize: 4,
			prepare: func(page []domain.SyncChangeEvent) []domain.SyncChangeEvent {
				page[2].EntryID++
				return page
			}, errorText: "序号 20 的增量事件失败"},
		{name: "replacement cannot use metadata payload", position: 17, highWater: 21, pageSize: 4,
			prepare: func(page []domain.SyncChangeEvent) []domain.SyncChangeEvent {
				page[3].Payload = page[2].Payload
				return page
			}, errorText: "序号 21 的增量事件失败"},
		{name: "unknown type with valid payload", position: 17, highWater: 21, pageSize: 4,
			prepare: func(page []domain.SyncChangeEvent) []domain.SyncChangeEvent {
				page[2].EventType = "UNKNOWN_EVENT"
				return page
			}, errorText: "序号 20 的增量事件失败"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			page := slices.Clone(events)
			if scenario.prepare != nil {
				page = scenario.prepare(page)
			}
			err := ValidateSyncChangePage(scenario.position, scenario.highWater, scenario.pageSize, page)
			if scenario.errorText == "" {
				if err != nil {
					t.Fatalf("完整页校验失败：%v", err)
				}
			} else if !errors.Is(err, domain.ErrInvalidSyncChangePage) || !strings.Contains(err.Error(), scenario.errorText) {
				t.Fatalf("期望包含 %q 的增量页错误，实际为 %v", scenario.errorText, err)
			}
		})
	}
}
