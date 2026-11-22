package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartUploadSessionCandidateBatchScannerSharesBudgetAndResumes(t *testing.T) {
	first, second, third := sessionBatchTargets()
	type storageRead struct {
		Key, Marker string
	}
	var reads []storageRead
	var afterIDs []uint64
	scanner := NewMultipartUploadSessionCandidateScanner("local_minio", "fixture-bucket", func(_ context.Context, key, marker string, limit int) (*domain.MultipartUploadSessionPage, error) {
		if limit != 1 {
			t.Fatal("会话分页大小改变", limit)
		}
		reads = append(reads, storageRead{key, marker})
		if key == second.ObjectKey && marker == "" {
			return &domain.MultipartUploadSessionPage{Sessions: []domain.MultipartUploadSession{
				{ObjectKey: key, UploadID: "middle", InitiatedAt: time.Unix(1, 0)},
			}, NextUploadID: "middle"}, nil
		}
		return &domain.MultipartUploadSessionPage{Sessions: []domain.MultipartUploadSession{
			{ObjectKey: key, UploadID: "tail", InitiatedAt: time.Unix(1, 0)},
		}}, nil
	}, func(_ context.Context, _, _ string, target domain.MultipartUploadSessionScanTarget, session domain.MultipartUploadSession, grace time.Duration) (domain.MultipartUploadSessionCleanupFacts, error) {
		if session.ObjectKey != target.ObjectKey || grace != time.Minute {
			t.Fatal("对象范围或宽限期改变")
		}
		return domain.MultipartUploadSessionCleanupFacts{GraceElapsed: true}, nil
	})
	batch := NewMultipartUploadSessionCandidateBatchScanner(scanner, func(_ context.Context, provider, bucket string, afterID uint64, limit int) (*domain.MultipartUploadSessionScanPage, error) {
		if provider != "local_minio" || bucket != "fixture-bucket" || limit != 3 {
			t.Fatal("数据库查询范围或页大小不符")
		}
		afterIDs = append(afterIDs, afterID)
		page := &domain.MultipartUploadSessionScanPage{}
		for _, target := range []domain.MultipartUploadSessionScanTarget{first, second, third} {
			if target.ObjectID > afterID {
				page.Targets = append(page.Targets, target)
			}
		}
		return page, nil
	})
	round, err := batch.ScanObjectPage(t.Context(), MultipartUploadSessionCandidateCursor{}, 3, 1, 2, time.Minute)
	if err != nil || round.Done || round.PagesScanned != 2 || len(round.Objects) != 2 ||
		round.Next != (MultipartUploadSessionCandidateCursor{ObjectID: second.ObjectID, UploadID: "middle"}) ||
		round.Objects[0].Target != first || round.Objects[1].Target != second || len(round.Objects[1].Scan.Candidates) != 1 {
		t.Fatal("跨对象预算耗尽时未记录对象和会话位置", round, err)
	}
	resumed, err := batch.ScanObjectPage(t.Context(), round.Next, 3, 1, 1, time.Minute)
	if err != nil || resumed.Done || resumed.PagesScanned != 1 || len(resumed.Objects) != 1 ||
		resumed.Next != (MultipartUploadSessionCandidateCursor{ObjectID: second.ObjectID}) || resumed.Objects[0].Target != second {
		t.Fatal("恢复后未在对象边界暂停", resumed, err)
	}
	last, err := batch.ScanObjectPage(t.Context(), resumed.Next, 3, 1, 1, time.Minute)
	if err != nil || !last.Done || last.PagesScanned != 1 || len(last.Objects) != 1 || last.Objects[0].Target != third ||
		!reflect.DeepEqual(afterIDs, []uint64{0, second.ObjectID - 1, second.ObjectID}) ||
		!reflect.DeepEqual(reads, []storageRead{{first.ObjectKey, ""}, {second.ObjectKey, ""}, {second.ObjectKey, "middle"}, {third.ObjectKey, ""}}) {
		t.Fatal("对象或会话重复、漏读或用了其他对象的游标", last, afterIDs, reads, err)
	}
}

func TestMultipartUploadSessionCandidateBatchScannerHandlesObjectPageEdges(t *testing.T) {
	first, second, _ := sessionBatchTargets()
	readFailure := errors.New("fixture target storage failure")
	for _, fixture := range []struct {
		name          string
		page          *domain.MultipartUploadSessionScanPage
		want          MultipartUploadSessionCandidateCursor
		wantErr       error
		wantKeys      []string
		wantDone      bool
		wantCompleted int
	}{
		{name: "filtered page", page: &domain.MultipartUploadSessionScanPage{NextAfterObjectID: 30}, want: MultipartUploadSessionCandidateCursor{ObjectID: 30}},
		{name: "empty end", page: &domain.MultipartUploadSessionScanPage{}, want: MultipartUploadSessionCandidateCursor{ObjectID: first.ObjectID}, wantDone: true},
		{name: "stale object cursor", page: &domain.MultipartUploadSessionScanPage{Targets: []domain.MultipartUploadSessionScanTarget{second}},
			want: MultipartUploadSessionCandidateCursor{ObjectID: second.ObjectID}, wantDone: true, wantCompleted: 1, wantKeys: []string{second.ObjectKey}},
		{name: "object query cursor moves backward", page: &domain.MultipartUploadSessionScanPage{Targets: []domain.MultipartUploadSessionScanTarget{second}, NextAfterObjectID: first.ObjectID},
			want: MultipartUploadSessionCandidateCursor{ObjectID: first.ObjectID, UploadID: "old"}, wantErr: domain.ErrInvalidStoredObjectData},
		{name: "second object fails", page: &domain.MultipartUploadSessionScanPage{Targets: []domain.MultipartUploadSessionScanTarget{first, second}},
			want: MultipartUploadSessionCandidateCursor{ObjectID: first.ObjectID}, wantErr: readFailure, wantCompleted: 1, wantKeys: []string{first.ObjectKey, second.ObjectKey}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var readKeys []string
			scanner := NewMultipartUploadSessionCandidateScanner("local_minio", "fixture-bucket", func(_ context.Context, key, marker string, _ int) (*domain.MultipartUploadSessionPage, error) {
				readKeys = append(readKeys, key)
				if (key == first.ObjectKey && marker != "old") || (key == second.ObjectKey && marker != "") {
					t.Fatal("未按对象归属使用原会话游标", key, marker)
				}
				if fixture.wantErr == readFailure && key == second.ObjectKey {
					return nil, readFailure
				}
				return &domain.MultipartUploadSessionPage{}, nil
			}, func(context.Context, string, string, domain.MultipartUploadSessionScanTarget, domain.MultipartUploadSession, time.Duration) (domain.MultipartUploadSessionCleanupFacts, error) {
				t.Fatal("空会话页不应触发仓储复核")
				return domain.MultipartUploadSessionCleanupFacts{}, nil
			})
			batch := NewMultipartUploadSessionCandidateBatchScanner(scanner, func(_ context.Context, _, _ string, afterID uint64, _ int) (*domain.MultipartUploadSessionScanPage, error) {
				if afterID != first.ObjectID-1 {
					t.Fatal("没有从未读完的对象重新查询", afterID)
				}
				return fixture.page, nil
			})
			start := MultipartUploadSessionCandidateCursor{ObjectID: first.ObjectID, UploadID: "old"}
			got, err := batch.ScanObjectPage(t.Context(), start, 2, 1, 2, time.Minute)
			if !errors.Is(err, fixture.wantErr) || got.PagesScanned != fixture.wantCompleted ||
				got.Next != fixture.want || got.Done != fixture.wantDone || !reflect.DeepEqual(readKeys, fixture.wantKeys) {
				t.Fatal("对象空页、断点或错误处理不符", got, readKeys, err)
			}
		})
	}
}

func sessionBatchTargets() (first, second, third domain.MultipartUploadSessionScanTarget) {
	first = domain.MultipartUploadSessionScanTarget{ObjectID: 5, UserID: 7, TaskID: 11, ObjectKey: "users/7/objects/4b351d22-0cab-4a8d-89cc-9fe9d0641e25"}
	second = domain.MultipartUploadSessionScanTarget{ObjectID: 9, UserID: 7, TaskID: 13, ObjectKey: "users/7/objects/4b351d22-0cab-4a8d-89cc-9fe9d0641e26"}
	third = domain.MultipartUploadSessionScanTarget{ObjectID: 20, UserID: 7, TaskID: 15, ObjectKey: "users/7/objects/4b351d22-0cab-4a8d-89cc-9fe9d0641e27"}
	return first, second, third
}
