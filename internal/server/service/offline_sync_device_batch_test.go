package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestOfflineSyncDeviceBatchRejectsInvalidRoundOrPage(t *testing.T) {
	cutoff := time.Date(2026, 8, 25, 12, 0, 0, 15625000, time.UTC)
	valid := domain.OfflineSyncDeviceCandidate{
		UserID: 7, DeviceID: 12, Generation: domain.SyncCursorGeneration{1}, LastSeenAt: cutoff.Add(-time.Microsecond),
	}
	atCutoff := valid
	atCutoff.DeviceID = 13
	atCutoff.LastSeenAt = cutoff
	scanFailure := errors.New("合成读取失败")

	for _, fixture := range []struct {
		name      string
		cutoff    time.Time
		readError error
		page      []domain.OfflineSyncDeviceCandidate
		pageError error
		wantError error
		wantLists int
	}{
		{name: "clock failure", readError: scanFailure, wantError: scanFailure},
		{name: "zero cutoff", wantError: domain.ErrInvalidSyncDeviceData},
		{name: "submicrosecond cutoff", cutoff: cutoff.Add(time.Nanosecond), wantError: domain.ErrInvalidSyncDeviceData},
		{name: "scan failure", cutoff: cutoff, pageError: scanFailure, wantError: scanFailure, wantLists: 1},
		{name: "page exceeds limit", cutoff: cutoff, page: []domain.OfflineSyncDeviceCandidate{valid, valid, valid},
			wantError: domain.ErrInvalidSyncDeviceData, wantLists: 1},
		{name: "duplicate ID", cutoff: cutoff, page: []domain.OfflineSyncDeviceCandidate{valid, valid},
			wantError: domain.ErrInvalidSyncDeviceData, wantLists: 1},
		{name: "invalid tail makes entire page unusable", cutoff: cutoff, page: []domain.OfflineSyncDeviceCandidate{valid, atCutoff},
			wantError: domain.ErrInvalidSyncDeviceData, wantLists: 1},
		{name: "empty page wraps cursor", cutoff: cutoff, wantLists: 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			reads, lists := 0, 0
			batch := NewOfflineSyncDeviceBatch(func(context.Context) (time.Time, error) {
				reads++
				return fixture.cutoff, fixture.readError
			}, func(_ context.Context, received time.Time, after uint64, limit int) ([]domain.OfflineSyncDeviceCandidate, error) {
				lists++
				if received != cutoff || after != 11 || limit != 2 {
					t.Fatalf("扫描输入不符：%v、%d、%d", received, after, limit)
				}
				return fixture.page, fixture.pageError
			}, func(context.Context, time.Time, domain.OfflineSyncDeviceCandidate) error {
				t.Fatal("空页或无效结果进入了单条标记")
				return nil
			})
			for _, bounds := range [][2]int{{0, 2}, {2, 0}, {101, 2}, {2, 101}} {
				if _, err := batch.RunOnce(t.Context(), 11, bounds[0], bounds[1]); err == nil || reads != 0 {
					t.Fatalf("批量或页大小不合法仍读取了截止点：%v", bounds)
				}
			}
			result, err := batch.RunOnce(t.Context(), 11, 2, 3)
			wantAfter := uint64(11)
			if fixture.wantError == nil {
				wantAfter = 0
			}
			if reads != 1 || lists != fixture.wantLists || !errors.Is(err, fixture.wantError) ||
				result.NextAfterDeviceID != wantAfter || result.Scanned != 0 || result.Marked != 0 || len(result.CandidateErrors) != 0 {
				t.Fatalf("空轮或失败轮次结果不符：%+v，错误 %v，读钟 %d 次，扫描 %d 次", result, err, reads, lists)
			}
		})
	}
}

func TestOfflineSyncDeviceBatchPreservesProgressOnStop(t *testing.T) {
	const start = 11
	cutoff := time.Date(2026, 8, 25, 12, 0, 0, 15625000, time.UTC)
	scanFailure := errors.New("后续页读取失败")
	uncertain := errors.Join(domain.ErrUserTransactionCommitUnconfirmed, context.Canceled)

	for _, fixture := range []struct {
		name        string
		failScan    bool
		cancelError error
		wantMarks   int
		wantScanned int
		wantAfter   uint64
		wantFailure error
	}{
		{name: "later scan fails", failScan: true, wantMarks: 1, wantScanned: 1, wantAfter: 12},
		{name: "cancel during second mark", cancelError: context.Canceled, wantMarks: 1, wantScanned: 2, wantAfter: 12},
		{name: "second mark commits before cancellation", wantMarks: 2, wantScanned: 2, wantAfter: 13},
		{name: "unknown commit is retained as a failure", cancelError: uncertain, wantMarks: 1, wantScanned: 2, wantAfter: 13, wantFailure: uncertain},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			batch := NewOfflineSyncDeviceBatch(func(context.Context) (time.Time, error) { return cutoff, nil },
				func(_ context.Context, received time.Time, after uint64, _ int) ([]domain.OfflineSyncDeviceCandidate, error) {
					if received != cutoff {
						t.Fatal("分页截止点发生变化")
					}
					if fixture.failScan && after == 12 {
						return nil, scanFailure
					}
					if after != start {
						t.Fatalf("发生了意外的续页调用：%d", after)
					}
					first := domain.OfflineSyncDeviceCandidate{
						UserID: 7, DeviceID: 12, Generation: domain.SyncCursorGeneration{1}, LastSeenAt: cutoff.Add(-time.Microsecond),
					}
					if fixture.failScan {
						return []domain.OfflineSyncDeviceCandidate{first}, nil
					}
					second := first
					second.DeviceID = 13
					return []domain.OfflineSyncDeviceCandidate{first, second}, nil
				}, func(_ context.Context, received time.Time, candidate domain.OfflineSyncDeviceCandidate) error {
					calls++
					if received != cutoff || candidate.DeviceID != uint64(start+calls) {
						t.Fatal("提交未收到原候选与截止点")
					}
					if calls == 2 {
						cancel()
						return fixture.cancelError
					}
					return nil
				})

			pageSize := 2
			wantError := error(context.Canceled)
			if fixture.failScan {
				pageSize, wantError = 1, scanFailure
			}
			result, err := batch.RunOnce(ctx, start, 3, pageSize)
			wantCalls := 2
			if fixture.failScan {
				wantCalls = 1
			}
			if !errors.Is(err, wantError) || calls != wantCalls || result.Marked != fixture.wantMarks ||
				result.Scanned != fixture.wantScanned || result.NextAfterDeviceID != fixture.wantAfter {
				t.Fatalf("本轮停止时未保留正确进度：%+v，错误 %v，提交 %d 次", result, err, calls)
			}
			if fixture.wantFailure == nil {
				if len(result.CandidateErrors) != 0 {
					t.Fatalf("取消或读页错误被误记为单项失败：%+v", result.CandidateErrors)
				}
			} else if len(result.CandidateErrors) != 1 || result.CandidateErrors[0].DeviceID != 13 ||
				!errors.Is(result.CandidateErrors[0].Err, fixture.wantFailure) {
				t.Fatalf("未保留提交结果未知的设备：%+v", result.CandidateErrors)
			}
		})
	}
}
