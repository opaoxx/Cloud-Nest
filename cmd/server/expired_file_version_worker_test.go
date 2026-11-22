package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerExpiredFileVersionWorkerReportsAndWaitsForStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 首轮扫描失败，第二轮在一小时后启动并等待取消。
		var logs bytes.Buffer
		secondStarted, canceled := make(chan struct{}), make(chan struct{})
		release, stopped := make(chan struct{}), make(chan struct{})
		rounds := 0
		start := time.Now()
		stop := startServerExpiredFileVersionWorker(t.Context(), func(ctx context.Context, limit int) (service.ExpiredFileVersionBatchResult, error) {
			if limit != 100 {
				t.Error("到期历史每轮上限应为 100", limit)
			}
			rounds++
			if rounds == 1 {
				if time.Since(start) != 0 {
					t.Error("首轮应立即执行")
				}
				return service.ExpiredFileVersionBatchResult{}, errors.New("fixture private scan detail")
			}
			close(secondStarted)
			<-ctx.Done()
			close(canceled)
			<-release
			time.Sleep(2 * time.Second)
			return service.ExpiredFileVersionBatchResult{
				Scanned: 5, Settled: 1, StaleCandidates: 1,
				CandidateErrors: []service.ExpiredFileVersionCandidateError{
					{UserID: 7, VersionID: 51, Err: fmt.Errorf("fixture private data detail: %w", domain.ErrInvalidFileVersionData)},
					{UserID: 8, VersionID: 52, Err: errors.Join(ctx.Err(), errors.New("fixture private rollback detail"))},
					{UserID: 8, VersionID: 53, Err: ctx.Err()},
				},
			}, fmt.Errorf("fixture private shutdown detail: %w", ctx.Err())
		}, logging.New(&logs))
		<-secondStarted
		if time.Since(start) != time.Hour {
			t.Error("第二轮应在完整一小时后执行", time.Since(start))
		}

		// 2. 停止方法先取消，再等待在途轮次返回。
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

		// 3. 确认日志汇总、固定错误码和最后一轮的退出顺序。
		type workerLog struct {
			Message         string `json:"msg"`
			Level           string `json:"level"`
			IntervalMS      int64  `json:"interval_ms"`
			BatchLimit      int    `json:"batch_limit"`
			DurationMS      int64  `json:"duration_ms"`
			Scanned         int    `json:"scanned"`
			Settled         int    `json:"settled"`
			StaleCandidates int    `json:"stale_candidates"`
			CandidateErrors int    `json:"candidate_errors"`
			UserID          uint64 `json:"user_id"`
			VersionID       uint64 `json:"version_id"`
			ErrorCode       string `json:"error_code"`
		}
		var actual []workerLog
		for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
			var entry workerLog
			if err := json.Unmarshal(line, &entry); err != nil {
				t.Fatal("无法解析 worker JSON 日志", err)
			}
			actual = append(actual, entry)
		}
		want := []workerLog{
			{Message: "历史到期结算 worker 已启动", Level: "INFO", IntervalMS: 3600000, BatchLimit: 100},
			{Message: "历史到期结算轮次失败", Level: "WARN", ErrorCode: "INTERNAL_ERROR"},
			{Message: "历史到期结算轮次结束", Level: "INFO"},
			{Message: "到期历史版本结算失败", Level: "WARN", UserID: 7, VersionID: 51, ErrorCode: "INVALID_FILE_VERSION_DATA"},
			{Message: "到期历史版本结算失败", Level: "WARN", UserID: 8, VersionID: 52, ErrorCode: "INTERNAL_ERROR"},
			{Message: "历史到期结算轮次结束", Level: "INFO", DurationMS: 2000, Scanned: 5, Settled: 1, StaleCandidates: 1, CandidateErrors: 3},
			{Message: "历史到期结算 worker 已停止", Level: "INFO"},
		}
		if rounds != 2 || !reflect.DeepEqual(actual, want) || strings.Contains(logs.String(), "fixture private") {
			t.Fatal("日志字段、错误过滤或退出顺序不符", rounds, logs.String())
		}
	})
}

func TestServerExpiredFileVersionWorkerWaitsForFinalReportOnServiceCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 在途轮次从父上下文收到取消，进入汇报但暂不写完日志。
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var logs bytes.Buffer
		gate := &serverWorkerReportGate{
			Handler: logging.New(&logs).Handler(), reportMessage: "历史到期结算轮次结束",
			reportStarted: make(chan struct{}), releaseReport: make(chan struct{}),
		}
		started, finished := make(chan struct{}), make(chan struct{})
		stop := startServerExpiredFileVersionWorker(ctx, func(roundContext context.Context, _ int) (service.ExpiredFileVersionBatchResult, error) {
			close(started)
			<-roundContext.Done()
			return service.ExpiredFileVersionBatchResult{Scanned: 1, Settled: 1}, roundContext.Err()
		}, slog.New(gate))
		<-started
		cancel()
		<-gate.reportStarted

		// 2. 汇报堵塞时停止方法也等待，最后汇报应在退出日志之前完成。
		go func() {
			stop()
			close(finished)
		}()
		synctest.Wait()
		select {
		case <-finished:
			t.Error("汇报尚未写完，停止方法已返回")
		default:
		}
		close(gate.releaseReport)
		<-finished
		logText := logs.String()
		report := strings.Index(logText, "历史到期结算轮次结束")
		stopped := strings.Index(logText, "历史到期结算 worker 已停止")
		if report < 0 || stopped <= report || !strings.Contains(logText, `"settled":1`) || strings.Contains(logText, `"level":"WARN"`) {
			t.Fatal("父上下文取消后的汇报不符", logText)
		}
	})
}
