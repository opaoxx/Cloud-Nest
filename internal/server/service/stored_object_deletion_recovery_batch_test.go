package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestStoredObjectDeletionRecoveryBatchScan(t *testing.T) {
	scanFailure := errors.New("fixture recovery scan failure")
	for _, fixture := range []struct {
		name           string
		limit, scanned int
		scanError      error
		want           error
	}{
		{name: "empty scan", limit: 100},
		{name: "scan failed", limit: 1, scanError: scanFailure, want: scanFailure},
		{name: "scan exceeds limit", limit: 1, scanned: 2, want: domain.ErrInvalidStoredObjectData},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			scans := 0
			batch := NewStoredObjectDeletionRecoveryBatch(func(ctx context.Context, limit int) ([]domain.StoredObject, error) {
				scans++
				if ctx != t.Context() || limit != fixture.limit {
					t.Fatal("扫描收到的上下文或上限不符", limit)
				}
				return make([]domain.StoredObject, fixture.scanned), fixture.scanError
			}, func(context.Context, *domain.StoredObject) (*domain.StoredObject, error) {
				t.Fatal("空批次或扫描失败时不应调用处理")
				return nil, nil
			})
			for _, limit := range []int{0, 101} {
				if _, err := batch.RunOnce(t.Context(), limit); err == nil || scans != 0 {
					t.Fatal("无效上限进入了扫描", limit, err)
				}
			}
			summary, err := batch.RunOnce(t.Context(), fixture.limit)
			if scans != 1 || !errors.Is(err, fixture.want) || summary.Scanned != 0 ||
				summary.Completed != 0 || len(summary.CandidateErrors) != 0 {
				t.Fatal("扫描结果不符", scans, summary, err)
			}
		})
	}
}

func TestStoredObjectDeletionRecoveryBatchContinuesAfterFailure(t *testing.T) {
	deleting := domain.StoredObjectStatusDeleting
	candidates := []domain.StoredObject{
		{ID: 31, OwnerUserID: 7, Status: deleting},
		{ID: 15, OwnerUserID: 9, Status: deleting},
		{ID: 18, OwnerUserID: 9, Status: deleting},
	}
	failure := domain.ErrStoredObjectDeleteUnavailable
	var processed []uint64
	batch := NewStoredObjectDeletionRecoveryBatch(func(context.Context, int) ([]domain.StoredObject, error) {
		return candidates, nil
	}, func(_ context.Context, object *domain.StoredObject) (*domain.StoredObject, error) {
		processed = append(processed, object.ID)
		switch object.ID {
		case 31:
			return nil, failure
		case 15:
			return object, nil
		default:
			completed := *object
			completed.Status = domain.StoredObjectStatusDeleted
			return &completed, nil
		}
	})
	summary, err := batch.RunOnce(t.Context(), 3)
	if err != nil || !slices.Equal(processed, []uint64{31, 15, 18}) || summary.Scanned != 3 ||
		summary.Completed != 1 || len(summary.CandidateErrors) != 2 {
		t.Fatal("逐项恢复与继续处理的结果不符", processed, summary, err)
	}
	for index, want := range []error{failure, domain.ErrInvalidStoredObjectData} {
		failure := summary.CandidateErrors[index]
		if failure.ObjectID != candidates[index].ID || failure.OwnerUserID != candidates[index].OwnerUserID || !errors.Is(failure.Err, want) {
			t.Fatal("失败记录未保留对象身份或原因", failure)
		}
	}
}

func TestStoredObjectDeletionRecoveryBatchPreservesCompletedOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	batch := NewStoredObjectDeletionRecoveryBatch(func(context.Context, int) ([]domain.StoredObject, error) {
		return []domain.StoredObject{{ID: 31, OwnerUserID: 7}, {ID: 32, OwnerUserID: 8}}, nil
	}, func(_ context.Context, object *domain.StoredObject) (*domain.StoredObject, error) {
		if object.ID != 31 {
			t.Fatal("取消后仍处理了下一项", object.ID)
		}
		cancel()
		completed := *object
		completed.Status = domain.StoredObjectStatusDeleted
		return &completed, nil
	})
	summary, err := batch.RunOnce(ctx, 2)
	if !errors.Is(err, context.Canceled) || summary.Scanned != 2 || summary.Completed != 1 || len(summary.CandidateErrors) != 0 {
		t.Fatal("取消后的已完成计数不符", summary, err)
	}
}
