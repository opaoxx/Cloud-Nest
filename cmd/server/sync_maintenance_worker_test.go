package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

type syncMaintenanceWorkerStoreFixture struct {
	offlineSyncDeviceWorkerStoreFixture
	syncChangePruneWorkerStoreFixture
}

func TestServerSyncMaintenanceWorkerRunsOrderedRoundsAndResumes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 首轮用满两种预算，第二轮离线扫描失败，第三轮从各自原位置继续。
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Hour+3*time.Minute)
		defer cancel()
		var logs bytes.Buffer
		start := time.Now()
		cutoff := time.Date(2026, time.August, 25, 12, 0, 0, 15625000, time.UTC)
		var offlineStarts, pruneStarts []time.Duration
		var devicePositions, userPositions []uint64
		var marked, pruned int
		fixture := syncMaintenanceWorkerStoreFixture{
			offlineSyncDeviceWorkerStoreFixture: offlineSyncDeviceWorkerStoreFixture{
				read: func(context.Context) (time.Time, error) {
					offlineStarts = append(offlineStarts, time.Since(start))
					return cutoff, nil
				},
				list: func(_ context.Context, stamp time.Time, after uint64, limit int) ([]domain.OfflineSyncDeviceCandidate, error) {
					devicePositions = append(devicePositions, after)
					if stamp != cutoff || limit != 50 {
						t.Errorf("离线重建页参数不符：%v，%d", stamp, limit)
					}
					switch len(offlineStarts) {
					case 1:
						firstDeviceBase := after
						if firstDeviceBase == 0 {
							firstDeviceBase = 1000
						}
						candidates := make([]domain.OfflineSyncDeviceCandidate, 0, limit)
						for offset := 1; offset <= limit; offset++ {
							candidates = append(candidates, domain.OfflineSyncDeviceCandidate{
								UserID: 7, DeviceID: firstDeviceBase + uint64(offset), LastSeenAt: cutoff.Add(-time.Microsecond),
								Generation: domain.SyncCursorGeneration{1},
							})
						}
						return candidates, nil
					case 2:
						return nil, errors.New("fixture private offline scan error")
					default:
						return nil, nil
					}
				},
				mark: func(_ context.Context, stamp time.Time, candidate domain.OfflineSyncDeviceCandidate) error {
					marked++
					if stamp != cutoff || candidate.DeviceID != 1000+uint64(marked) {
						t.Errorf("离线候选顺序不符：%d，%v", candidate.DeviceID, stamp)
					}
					return nil
				},
			},
			syncChangePruneWorkerStoreFixture: syncChangePruneWorkerStoreFixture{
				read: func(context.Context) (time.Time, error) {
					pruneStarts = append(pruneStarts, time.Since(start))
					if marked != 100 || strings.Count(logs.String(), "离线设备重建轮次结束") != len(offlineStarts) {
						t.Error("离线重建及汇报完成前进入了裁剪")
					}
					return cutoff, nil
				},
				list: func(_ context.Context, stamp time.Time, after uint64, limit int) (domain.SyncChangePruneCandidatePage, error) {
					userPositions = append(userPositions, after)
					if stamp != cutoff || limit != 50 {
						t.Errorf("日志裁剪页参数不符：%v，%d", stamp, limit)
					}
					if len(pruneStarts) == 2 {
						return domain.SyncChangePruneCandidatePage{NextAfterUserID: after}, nil
					}
					page := domain.SyncChangePruneCandidatePage{NextAfterUserID: after + 50, ScannedCount: 50}
					if after == 0 {
						page.UserIDs = []uint64{7}
					}
					return page, nil
				},
				prune: func(_ context.Context, userID uint64, stamp time.Time, limit int) (domain.SyncChangePruneRange, error) {
					pruned++
					if userID != 7 || stamp != cutoff || limit != 100 {
						t.Errorf("单账号裁剪参数不符：%d，%v，%d", userID, stamp, limit)
					}
					time.Sleep(2 * time.Minute)
					return domain.SyncChangePruneRange{FirstSequence: 18, LastSequence: 19, Count: 2}, nil
				},
			},
		}
		stop := startServerSyncMaintenanceWorker(ctx, fixture, logging.New(&logs))
		<-ctx.Done()
		stop()

		// 2. 单轮两阶段汇报后才等待一小时。跳过裁剪时账号游标保持原值。
		if !slices.Equal(offlineStarts, []time.Duration{0, time.Hour + 2*time.Minute, 2*time.Hour + 2*time.Minute}) ||
			!slices.Equal(pruneStarts, []time.Duration{0, 2*time.Hour + 2*time.Minute}) ||
			!slices.Equal(devicePositions, []uint64{0, 1050, 1100, 1100}) || !slices.Equal(userPositions, []uint64{0, 50, 100}) ||
			marked != 100 || pruned != 1 {
			t.Fatalf("间隔、续扫或处理数不符：离线 %v/%v，裁剪 %v/%v，处理 %d/%d",
				offlineStarts, devicePositions, pruneStarts, userPositions, marked, pruned)
		}
		logText := logs.String()
		for _, line := range []string{
			`"interval_ms":3600000,"offline_batch_limit":100,"offline_page_size":50,"scan_limit":100,"page_size":50,"account_delete_limit":100`,
			`"scanned":100,"marked":100,"stale_candidates":0,"candidate_errors":0,"next_after_device_id":1100`,
			`"scanned":100,"pruned_accounts":1,"deleted_events":2,"empty_accounts":0,"account_errors":0,"next_after_user_id":100`,
			`"msg":"离线设备重建轮次失败"`, `"error_code":"INTERNAL_ERROR"`,
			`"msg":"同步日志裁剪本轮跳过"`, `"next_after_user_id":100`, `"next_after_user_id":0`,
		} {
			if !strings.Contains(logText, line) {
				t.Fatalf("缺少预期汇报 %s：%s", line, logText)
			}
		}
		if strings.Count(logText, "离线设备重建轮次结束") != 3 || strings.Count(logText, "同步日志裁剪轮次结束") != 2 ||
			strings.Contains(logText, "fixture private") || !strings.Contains(logText, "同步维护 worker 已停止") {
			t.Fatalf("汇报数量或退出日志不符：%s", logText)
		}
	})
}

func TestServerSyncMaintenanceWorkerWaitsForStageReport(t *testing.T) {
	for _, fixture := range []struct {
		name          string
		reportMessage string
		wantPrune     bool
	}{
		{name: "stop during offline report", reportMessage: "离线设备重建轮次结束"},
		{name: "stop during prune report", reportMessage: "同步日志裁剪轮次结束", wantPrune: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// 1. 阻塞任一阶段的汇报，发出取消信号。
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var logs bytes.Buffer
				gate := &serverWorkerReportGate{
					Handler: logging.New(&logs).Handler(), reportMessage: fixture.reportMessage,
					reportStarted: make(chan struct{}), releaseReport: make(chan struct{}),
				}
				cutoff := time.Date(2026, time.August, 25, 12, 0, 0, 15625000, time.UTC)
				pruneStarted := false
				store := syncMaintenanceWorkerStoreFixture{
					offlineSyncDeviceWorkerStoreFixture: offlineSyncDeviceWorkerStoreFixture{
						read: func(context.Context) (time.Time, error) { return cutoff, nil },
						list: func(context.Context, time.Time, uint64, int) ([]domain.OfflineSyncDeviceCandidate, error) {
							return nil, nil
						},
					},
					syncChangePruneWorkerStoreFixture: syncChangePruneWorkerStoreFixture{
						read: func(context.Context) (time.Time, error) {
							pruneStarted = true
							return cutoff, nil
						},
						list: func(context.Context, time.Time, uint64, int) (domain.SyncChangePruneCandidatePage, error) {
							return domain.SyncChangePruneCandidatePage{}, nil
						},
					},
				}
				stop := startServerSyncMaintenanceWorker(ctx, store, slog.New(gate))
				<-gate.reportStarted
				cancel()
				stopped := make(chan struct{})
				go func() {
					stop()
					close(stopped)
				}()
				synctest.Wait()
				select {
				case <-stopped:
					t.Error("阶段汇报仍在等待，停止方法已完成")
				default:
				}

				// 2. 放行后记录停止，且取消发生在离线汇报期间时不进入裁剪。
				close(gate.releaseReport)
				<-stopped
				logText := logs.String()
				report := strings.Index(logText, fixture.reportMessage)
				stoppedLog := strings.Index(logText, "同步维护 worker 已停止")
				if pruneStarted != fixture.wantPrune || report < 0 || stoppedLog <= report ||
					strings.Contains(logText, `"level":"WARN"`) || strings.Contains(logText, `"level":"ERROR"`) {
					t.Fatalf("退出顺序或阶段执行不符：裁剪 %t，%s", pruneStarted, logText)
				}
			})
		})
	}
}
