package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerMultipartUploadSessionCleanupWorkerReportsRounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		const interval = 2 * time.Second
		const private = "fixture-private-storage-metadata"
		target := domain.MultipartUploadSessionScanTarget{TaskID: math.MaxUint64, ObjectID: 23, ObjectKey: private}
		var rounds atomic.Int32
		var starts []time.Duration
		start := time.Now()

		// 1. 首轮失败，第二轮回退，第三轮汇报一轮扫描和收尾结果。
		stop := startServerMultipartUploadSessionCleanupWorker(t.Context(), interval, func(context.Context, service.MultipartUploadSessionCleanupPosition) (service.MultipartUploadSessionCleanupRoundResult, error) {
			starts = append(starts, time.Since(start))
			switch rounds.Add(1) {
			case 1:
				return service.MultipartUploadSessionCleanupRoundResult{}, fmt.Errorf("%s: %w", private, domain.ErrUploadStorageUnavailable)
			case 2:
				return service.MultipartUploadSessionCleanupRoundResult{ScanRestarted: true}, nil
			default:
				return service.MultipartUploadSessionCleanupRoundResult{
					Scan: service.MultipartUploadSessionCandidateBatch{
						PagesScanned: 2, Objects: []service.MultipartUploadSessionObjectScan{{Target: target, Scan: service.MultipartUploadSessionCandidateScan{
							MultipartUploadSessionCandidatePage: service.MultipartUploadSessionCandidatePage{
								Scanned: 5, Failures: []service.MultipartUploadSessionScanFailure{{UploadID: private, Err: domain.ErrUploadTaskNotFound}},
							},
							InterruptedFailures: []service.MultipartUploadSessionScanFailure{{UploadID: private, Err: domain.ErrUploadStorageUnavailable}},
						}}},
					},
					Items: []service.MultipartUploadSessionCleanupItem{
						{Target: target, Session: domain.MultipartUploadSession{UploadID: private}, Err: errors.New(private)}, {}, {},
					},
					Confirmed: 1, Skipped: 1, Unconfirmed: 1, Failed: 1, Done: true,
				}, nil
			}
		}, logging.New(&logs))
		synctest.Wait()
		time.Sleep(interval - time.Nanosecond)
		synctest.Wait()
		if count := rounds.Load(); count != 1 {
			t.Error("下一轮在约定间隔之前启动", count)
		}
		time.Sleep(interval + time.Nanosecond)
		synctest.Wait()
		stop()
		if !slices.Equal(starts, []time.Duration{0, interval, 2 * interval}) {
			t.Fatal("worker 没有传入约定轮间隔", starts)
		}

		// 2. 解码实际 JSON，验证完整无符号 ID、计数、进度及固定错误码。
		type event struct {
			Message         string `json:"msg"`
			Code            string `json:"error_code"`
			TaskID          uint64 `json:"task_id"`
			ObjectID        uint64 `json:"object_id"`
			Interval        int    `json:"interval_ms"`
			ScannedObjects  int    `json:"scanned_objects"`
			ScannedPages    int    `json:"scanned_pages"`
			ScannedSessions int    `json:"scanned_sessions"`
			ScanErrors      int    `json:"scan_errors"`
			Processed       int    `json:"processed"`
			Skipped         int    `json:"skipped"`
			Confirmed       int    `json:"confirmed"`
			Unconfirmed     int    `json:"unconfirmed"`
			Failed          int    `json:"failed"`
			ScanRestarted   bool   `json:"scan_restarted"`
			Done            bool   `json:"done"`
		}
		logText := logs.String()
		decoder := json.NewDecoder(&logs)
		var events []event
		for {
			var entry event
			err := decoder.Decode(&entry)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, entry)
		}
		want := []event{
			{Message: "分片会话收尾 worker 已启动", Interval: 2000},
			{Message: "分片会话收尾轮次失败", Code: "STORAGE_UNAVAILABLE"},
			{Message: "分片会话收尾轮次结束"},
			{Message: "分片会话收尾轮次结束", ScanRestarted: true},
			{Message: "分片会话扫描复核失败", TaskID: math.MaxUint64, ObjectID: 23, Code: "UPLOAD_NOT_FOUND"},
			{Message: "分片会话扫描复核失败", TaskID: math.MaxUint64, ObjectID: 23, Code: "STORAGE_UNAVAILABLE"},
			{Message: "分片会话候选收尾失败", TaskID: math.MaxUint64, ObjectID: 23, Code: "INTERNAL_ERROR"},
			{Message: "分片会话收尾轮次结束", ScannedObjects: 1, ScannedPages: 2, ScannedSessions: 5, ScanErrors: 2,
				Processed: 3, Skipped: 1, Confirmed: 1, Unconfirmed: 1, Failed: 1, Done: true},
			{Message: "分片会话收尾 worker 已停止"},
		}
		if !slices.Equal(events, want) || strings.Contains(logText, private) || strings.Contains(logText, `"upload_id":`) ||
			strings.Contains(logText, `"object_key":`) {
			t.Fatal("轮次汇报内容、顺序或存储标识不符", logText)
		}
	})
}

func TestStopServerMultipartUploadSessionCleanupWorkerWaitsForActiveRound(t *testing.T) {
	for _, parentStops := range []bool{false, true} {
		t.Run(fmt.Sprint("parent stops ", parentStops), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var logs bytes.Buffer
				started, canceled, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
				stop := startServerMultipartUploadSessionCleanupWorker(ctx, time.Second, func(roundContext context.Context, _ service.MultipartUploadSessionCleanupPosition) (service.MultipartUploadSessionCleanupRoundResult, error) {
					close(started)
					<-roundContext.Done()
					close(canceled)
					<-release
					stopError := roundContext.Err()
					return service.MultipartUploadSessionCleanupRoundResult{Confirmed: 1, Failed: 2,
						Items: []service.MultipartUploadSessionCleanupItem{
							{Target: domain.MultipartUploadSessionScanTarget{TaskID: 19}, Err: stopError},
							{Target: domain.MultipartUploadSessionScanTarget{TaskID: 20}, Err: errors.Join(stopError, errors.New("fixture private observation error"))},
						}}, stopError
				}, logging.New(&logs))
				<-started
				if parentStops {
					cancel()
					<-canceled
				}
				go func() {
					stop()
					close(stopped)
				}()
				<-canceled
				synctest.Wait()
				select {
				case <-stopped:
					t.Error("在途轮次被挡住时停止方法已返回")
				default:
				}
				close(release)
				<-stopped
				stop()
				logged := logs.String()
				report := strings.Index(logged, "分片会话收尾轮次结束")
				exit := strings.Index(logged, "分片会话收尾 worker 已停止")
				if report < 0 || exit <= report || strings.Count(logged, "分片会话候选收尾失败") != 1 ||
					!strings.Contains(logged, `"task_id":20`) || !strings.Contains(logged, `"error_code":"INTERNAL_ERROR"`) ||
					strings.Contains(logged, `"task_id":19`) || strings.Contains(logged, "fixture private") ||
					strings.Contains(logged, "分片会话收尾轮次失败") || strings.Contains(logged, "分片会话收尾 worker 异常退出") {
					t.Fatal("停止顺序、纯取消或独立故障的汇报不符", logged)
				}
			})
		})
	}
}
