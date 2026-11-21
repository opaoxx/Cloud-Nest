package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestMultipartUploadInitializationBatchScan(t *testing.T) {
	scanError := errors.New("fixture scan failure")
	maximum := domain.MaxMultipartUploadInitializationCandidates
	for _, test := range []struct {
		name      string
		limit     int
		count     int
		scanError error
		wantError error
		wantScans int
		cancelled bool
	}{
		{name: "empty at maximum limit", limit: maximum, wantScans: 1},
		{name: "zero limit", limit: 0},
		{name: "over maximum", limit: maximum + 1},
		{name: "oversized list", limit: 1, count: 2, wantError: domain.ErrInvalidUploadTaskData, wantScans: 1},
		{name: "scan error", limit: 1, scanError: scanError, wantError: scanError, wantScans: 1},
		{name: "cancel before scan", limit: 1, wantError: context.Canceled, cancelled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancelled {
				cancel()
			}
			scans := 0
			batch := NewMultipartUploadInitializationBatch(func(scanContext context.Context, limit int) ([]domain.MultipartUploadInitializationCandidate, error) {
				scans++
				if scanContext != ctx || limit != test.limit {
					t.Fatal("扫描收到错误的上下文或上限", limit)
				}
				return make([]domain.MultipartUploadInitializationCandidate, test.count), test.scanError
			}, func(context.Context, domain.MultipartUploadInitializationCandidate, string) (*domain.UploadTask, error) {
				t.Fatal("扫描无有效结果时调用了单条处理")
				return nil, nil
			})
			summary, err := batch.RunOnce(ctx, test.limit, "fixture-worker")
			if test.limit < 1 || test.limit > maximum {
				if err == nil {
					t.Fatal("接受了非法批量大小")
				}
			} else if !errors.Is(err, test.wantError) {
				t.Fatal("扫描返回了错误的原因", err)
			}
			if scans != test.wantScans || !reflect.DeepEqual(summary, MultipartUploadInitializationBatchResult{}) {
				t.Fatal("扫描次数或空汇总不符", scans, summary)
			}
		})
	}
}

func TestMultipartUploadInitializationBatchSummarizesInOrder(t *testing.T) {
	ctx := t.Context()
	failure := errors.New("fixture candidate failure")
	uploadID := "fixture-session"
	now := time.Now().UTC()
	retryAt := now.Add(time.Second)
	candidates := []domain.MultipartUploadInitializationCandidate{
		{TaskID: 11, UserID: 7, StagedObjectID: 21, LeaseVersion: 3},
		{TaskID: 12, UserID: 8, StagedObjectID: 22, LeaseVersion: 4},
		{TaskID: 13, UserID: 7, StagedObjectID: 23, LeaseVersion: 5},
		{TaskID: 14, UserID: 8, StagedObjectID: 24, LeaseVersion: 6},
	}
	var processed []domain.MultipartUploadInitializationCandidate
	batch := NewMultipartUploadInitializationBatch(func(context.Context, int) ([]domain.MultipartUploadInitializationCandidate, error) {
		return candidates, nil
	}, func(callContext context.Context, candidate domain.MultipartUploadInitializationCandidate, worker string) (*domain.UploadTask, error) {
		processed = append(processed, candidate)
		if callContext != ctx || worker != "fixture-worker" {
			t.Fatal("未转发上下文或 worker 标识")
		}
		task := &domain.UploadTask{ID: candidate.TaskID, UserID: candidate.UserID, StagedObjectID: candidate.StagedObjectID,
			LeaseVersion: candidate.LeaseVersion + 1, UploadMode: "MULTIPART", UpdatedAt: now}
		switch candidate.TaskID {
		case 11:
			return nil, failure
		case 12:
			task.Status, task.StorageUploadID = domain.UploadStatusUploading, &uploadID
		case 13:
			task.Status, task.NextAttemptAt = domain.UploadStatusCreating, &retryAt
		case 14:
			task.Status, task.StorageUploadID = domain.UploadStatusUploading, &uploadID
			task.LeaseVersion--
		}
		return task, nil
	})
	summary, err := batch.RunOnce(ctx, len(candidates), "fixture-worker")
	if err != nil || !reflect.DeepEqual(processed, candidates) || summary.Scanned != 4 ||
		summary.Bound != 1 || summary.RetriesScheduled != 1 || len(summary.CandidateErrors) != 2 {
		t.Fatal("扫描顺序或批次汇总不符", processed, summary, err)
	}
	first, second := summary.CandidateErrors[0], summary.CandidateErrors[1]
	if first.TaskID != 11 || !errors.Is(first.Err, failure) || second.TaskID != 14 || !errors.Is(second.Err, domain.ErrInvalidUploadTaskData) {
		t.Fatal("逐项错误未保留正确的任务和原因", summary.CandidateErrors)
	}
}

func TestMultipartUploadInitializationBatchPreservesResultsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := domain.MultipartUploadInitializationCandidate{TaskID: 11, UserID: 7, StagedObjectID: 21, LeaseVersion: 3}
	second := domain.MultipartUploadInitializationCandidate{TaskID: 12, UserID: 7, StagedObjectID: 22}
	uploadID := "fixture-session"
	batch := NewMultipartUploadInitializationBatch(func(context.Context, int) ([]domain.MultipartUploadInitializationCandidate, error) {
		return []domain.MultipartUploadInitializationCandidate{first, second}, nil
	}, func(_ context.Context, candidate domain.MultipartUploadInitializationCandidate, _ string) (*domain.UploadTask, error) {
		if candidate != first {
			t.Fatal("取消后仍处理下一候选")
		}
		cancel()
		return &domain.UploadTask{ID: first.TaskID, UserID: first.UserID, StagedObjectID: first.StagedObjectID,
			UploadMode: "MULTIPART", LeaseVersion: 4, Status: domain.UploadStatusUploading, StorageUploadID: &uploadID}, nil
	})
	summary, err := batch.RunOnce(ctx, 2, "fixture-worker")
	if !errors.Is(err, context.Canceled) || summary.Scanned != 2 || summary.Bound != 1 ||
		summary.RetriesScheduled != 0 || len(summary.CandidateErrors) != 0 {
		t.Fatal("取消未保留已提交结果", summary, err)
	}
}
