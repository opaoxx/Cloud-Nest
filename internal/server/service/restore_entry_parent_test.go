package service

import (
	"errors"
	"math"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestSelectRestoreEntryParentDepthAndFallback(t *testing.T) {
	const rootID = uint64(math.MaxUint64)
	const folderID = uint64(math.MaxUint64 - 1)
	for _, scenario := range []struct {
		name            string
		parentID        uint64
		parentDepth     uint64
		trashedAncestor bool
		entryType       string
		relativeDepth   uint64
		nodeCount       uint64
		want            uint64
		wantError       error
	}{
		{name: "file at depth 32", parentID: folderID, parentDepth: 31, entryType: "FILE", nodeCount: 1, want: folderID},
		{name: "original parent already root", parentID: rootID, entryType: "FILE", nodeCount: 1, want: rootID},
		{name: "directory at original depth 32", parentID: folderID, parentDepth: 2, entryType: "DIR", relativeDepth: 29, nodeCount: 30, want: folderID},
		{name: "depth 33 at original location falls back", parentID: folderID, parentDepth: 2, entryType: "DIR", relativeDepth: 30, nodeCount: 31, want: rootID},
		{name: "trashed ancestor forces fallback even if depth fits", parentID: folderID, parentDepth: 2, trashedAncestor: true, entryType: "DIR", nodeCount: 1, want: rootID},
		{name: "fallback root fits exactly", parentID: folderID, parentDepth: 2, trashedAncestor: true, entryType: "DIR", relativeDepth: 31, nodeCount: 32, want: rootID},
		{name: "active original and root too deep", parentID: folderID, parentDepth: 2, entryType: "DIR", relativeDepth: 32, nodeCount: 33, wantError: domain.ErrRestoreTargetUnavailable},
		{name: "trashed original and root too deep", parentID: folderID, parentDepth: 2, trashedAncestor: true, entryType: "DIR", relativeDepth: 32, nodeCount: 33, wantError: domain.ErrRestoreTargetUnavailable},
		{name: "original root too deep", parentID: rootID, entryType: "DIR", relativeDepth: 32, nodeCount: 33, wantError: domain.ErrRestoreTargetUnavailable},
		{name: "wide but shallow restore", parentID: folderID, parentDepth: 2, entryType: "DIR", relativeDepth: 1, nodeCount: domain.MaxFileEntrySubtreeNodes + 1, want: folderID},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			originalParentID := scenario.parentID
			target := domain.FileEntry{ID: 13, UserID: 7, Revision: 1, Status: "TRASHED", EntryType: scenario.entryType, ParentID: &originalParentID}
			ancestors := RestoreEntryAncestors{RootEntryID: rootID, OriginalParentDepth: scenario.parentDepth, HasTrashedAncestor: scenario.trashedAncestor}
			stats := domain.EntrySubtreeStats{UserID: target.UserID, EntryID: target.ID, NodeCount: scenario.nodeCount, MaxRelativeDepth: scenario.relativeDepth}
			parentID, err := SelectRestoreEntryParent(&target, &ancestors, &stats)
			if parentID != scenario.want || !errors.Is(err, scenario.wantError) {
				t.Fatalf("选址结果为 %d, %v，期望 %d, %v", parentID, err, scenario.want, scenario.wantError)
			}
			if target.ParentID != &originalParentID || originalParentID != scenario.parentID {
				t.Fatal("选址改动了原父目录")
			}
		})
	}
}

func TestSelectRestoreEntryParentRejectsInvalidData(t *testing.T) {
	parentID := uint64(12)
	target := domain.FileEntry{ID: 13, UserID: 7, Revision: 1, Status: "TRASHED", EntryType: "DIR", ParentID: &parentID}
	ancestors := RestoreEntryAncestors{RootEntryID: 11, OriginalParentDepth: 1}
	stats := domain.EntrySubtreeStats{UserID: 7, EntryID: 13, NodeCount: 1}
	foreignStats, otherSourceStats, invalidCount, impossibleDepth := stats, stats, stats, stats
	foreignStats.UserID++
	otherSourceStats.EntryID++
	invalidCount.NodeCount = 2
	impossibleDepth.MaxRelativeDepth, impossibleDepth.NodeCount = math.MaxUint64, math.MaxUint64
	rootAsTarget, activeTarget, fileTarget := target, target, target
	rootAsTarget.ID = ancestors.RootEntryID
	activeTarget.Status = "ACTIVE"
	fileTarget.EntryType = "FILE"
	fileWithChild := domain.EntrySubtreeStats{UserID: 7, EntryID: 13, NodeCount: 2, MaxRelativeDepth: 1}

	for _, scenario := range []struct {
		name      string
		target    *domain.FileEntry
		ancestors *RestoreEntryAncestors
		stats     *domain.EntrySubtreeStats
	}{
		{"nil target", nil, &ancestors, &stats},
		{"nil ancestors", &target, nil, &stats},
		{"nil stats", &target, &ancestors, nil},
		{"target is actual root", &rootAsTarget, &ancestors, &stats},
		{"active target", &activeTarget, &ancestors, &stats},
		{"zero root", &target, &RestoreEntryAncestors{OriginalParentDepth: 1}, &stats},
		{"nonroot parent at depth zero", &target, &RestoreEntryAncestors{RootEntryID: 11}, &stats},
		{"root parent with nonzero depth", &target, &RestoreEntryAncestors{RootEntryID: parentID, OriginalParentDepth: 1}, &stats},
		{"trashed root parent", &target, &RestoreEntryAncestors{RootEntryID: parentID, HasTrashedAncestor: true}, &stats},
		{"parent depth cannot hold current target", &target, &RestoreEntryAncestors{RootEntryID: 11, OriginalParentDepth: domain.MaxFileEntryDepth}, &stats},
		{"foreign stats", &target, &ancestors, &foreignStats},
		{"stats for other source", &target, &ancestors, &otherSourceStats},
		{"two nodes with no edges", &target, &ancestors, &invalidCount},
		{"relative depth beyond valid statistics", &target, &ancestors, &impossibleDepth},
		{"file with child", &fileTarget, &ancestors, &fileWithChild},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			parentID, err := SelectRestoreEntryParent(scenario.target, scenario.ancestors, scenario.stats)
			if parentID != 0 || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatalf("异常输入应被拒绝，父目录为 %d，错误为 %v", parentID, err)
			}
		})
	}
}
