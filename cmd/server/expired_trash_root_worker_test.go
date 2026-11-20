package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerExpiredTrashRootWorkerReportsAndWaitsForStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 首轮扫描失败，第二轮在一小时后开始并等待停止信号。
		var logs bytes.Buffer
		secondStarted := make(chan struct{})
		canceled := make(chan struct{})
		release := make(chan struct{})
		stopped := make(chan struct{})
		rounds := 0
		start := time.Now()
		stop := startServerExpiredTrashRootWorker(t.Context(), func(ctx context.Context, limit int) (service.ExpiredTrashRootBatchResult, error) {
			if limit != 100 {
				t.Error("到期标记每轮上限应为 100", limit)
			}
			rounds++
			if rounds == 1 {
				if time.Since(start) != 0 {
					t.Error("首轮应立即执行", time.Since(start))
				}
				return service.ExpiredTrashRootBatchResult{}, errors.New("fixture private scan detail")
			}
			close(secondStarted)
			<-ctx.Done()
			close(canceled)
			<-release
			time.Sleep(2 * time.Second)
			return service.ExpiredTrashRootBatchResult{
				Scanned: 4, Marked: 1, StaleCandidates: 1,
				CandidateErrors: []service.PurgeRootCandidateError{
					{UserID: 7, RootID: 31, Err: fmt.Errorf("fixture private revision detail: %w", domain.ErrEntryRevisionExhausted)},
					{UserID: 8, RootID: 32, Err: ctx.Err()},
				},
			}, fmt.Errorf("fixture private shutdown detail: %w", ctx.Err())
		}, logging.New(&logs))
		<-secondStarted
		if time.Since(start) != time.Hour {
			t.Error("第二轮应在一小时后开始", time.Since(start))
		}

		// 2. 停止方法必须先发出取消，再等待在途轮次结束。
		go func() {
			stop()
			close(stopped)
		}()
		<-canceled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("在途轮次尚未结束，停止方法已返回")
		default:
		}
		close(release)
		<-stopped
		stop()

		// 3. 核对 JSON 日志字段、关停过滤和最后一轮汇报的顺序。
		type workerLog struct {
			Message         string `json:"msg"`
			Level           string `json:"level"`
			IntervalMS      int64  `json:"interval_ms"`
			BatchLimit      int    `json:"batch_limit"`
			DurationMS      int64  `json:"duration_ms"`
			Scanned         int    `json:"scanned"`
			Marked          int    `json:"marked"`
			StaleCandidates int    `json:"stale_candidates"`
			CandidateErrors int    `json:"candidate_errors"`
			UserID          uint64 `json:"user_id"`
			RootID          uint64 `json:"root_id"`
			ErrorCode       string `json:"error_code"`
		}
		var actual []workerLog
		for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
			var entry workerLog
			if err := json.Unmarshal(line, &entry); err != nil {
				t.Fatal("无法解析汇报日志", err)
			}
			actual = append(actual, entry)
		}
		want := []workerLog{
			{Message: "回收站到期标记 worker 已启动", Level: "INFO", IntervalMS: 3600000, BatchLimit: 100},
			{Message: "回收站到期标记轮次失败", Level: "WARN", ErrorCode: "INTERNAL_ERROR"},
			{Message: "回收站到期标记轮次结束", Level: "INFO"},
			{Message: "回收站到期根项标记失败", Level: "WARN", UserID: 7, RootID: 31, ErrorCode: "ENTRY_REVISION_EXHAUSTED"},
			{Message: "回收站到期标记轮次结束", Level: "INFO", DurationMS: 2000, Scanned: 4, Marked: 1, StaleCandidates: 1, CandidateErrors: 2},
			{Message: "回收站到期标记 worker 已停止", Level: "INFO"},
		}
		if rounds != 2 || !reflect.DeepEqual(actual, want) || strings.Contains(logs.String(), "fixture private") {
			t.Fatal("到期标记日志或退出顺序不符", rounds, logs.String())
		}
	})
}

func TestServerExpiredTrashRootWorkerStopsWithServiceContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 在首轮中等待父级服务上下文的取消信号。
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var logs bytes.Buffer
		started := make(chan struct{})
		result := make(chan error, 1)
		stop := startServerExpiredTrashRootWorker(ctx, func(roundContext context.Context, _ int) (service.ExpiredTrashRootBatchResult, error) {
			close(started)
			<-roundContext.Done()
			result <- roundContext.Err()
			return service.ExpiredTrashRootBatchResult{}, roundContext.Err()
		}, logging.New(&logs))
		<-started

		// 2. 父级取消应传到在途轮次，退出前仍有正常汇报。
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatal("服务上下文未传递取消", err)
		}
		stop()
		logText := logs.String()
		report := strings.Index(logText, "回收站到期标记轮次结束")
		stopped := strings.Index(logText, "回收站到期标记 worker 已停止")
		if report < 0 || stopped <= report || strings.Contains(logText, `"level":"WARN"`) {
			t.Fatal("服务停止时的汇报不符", logText)
		}
	})
}
