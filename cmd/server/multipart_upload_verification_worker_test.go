package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerMultipartUploadVerificationWorkerReportsRounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 首轮立即执行并模拟耗时扫描失败，第二轮汇报各类结果。
		const privateDetail = "fixture://private/object?credential=hidden"
		const ownerPrefix = "multipart-verification-"
		var logs bytes.Buffer
		var starts []time.Duration
		var firstOwner string
		startedAt := time.Now()
		stop := startServerMultipartUploadVerificationWorker(t.Context(), func(_ context.Context, limit int, owner string) (service.UploadVerificationBatchResult, error) {
			starts = append(starts, time.Since(startedAt))
			identifier, err := uuid.Parse(strings.TrimPrefix(owner, ownerPrefix))
			if limit != 10 || !strings.HasPrefix(owner, ownerPrefix) || len(owner) > 64 || err != nil || identifier.Version() != 4 {
				t.Error("扫描上限或租约持有者错误", limit, owner)
			}
			if len(starts) == 1 {
				firstOwner = owner
				time.Sleep(7 * time.Second)
				return service.UploadVerificationBatchResult{}, errors.New(privateDetail)
			}
			if owner != firstOwner {
				t.Error("同一 worker 更换了租约持有者", owner)
			}
			return service.UploadVerificationBatchResult{
				Scanned: 4, Published: 1, RetriesScheduled: 1,
				CandidateErrors: []service.UploadVerificationCandidateError{
					{TaskID: 19, Err: &domain.UploadVerificationChecksumMismatch{}},
					{TaskID: math.MaxUint64, Err: errors.New(privateDetail)},
				},
			}, nil
		}, logging.New(&logs))
		defer stop()
		synctest.Wait()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if !slices.Equal(starts, []time.Duration{0}) {
			t.Fatal("首轮未立即运行或轮次发生重叠", starts)
		}
		time.Sleep(7*time.Second - time.Nanosecond)
		synctest.Wait()
		if len(starts) != 1 {
			t.Fatal("首轮结束后未等待完整间隔", starts)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		stop()
		if !slices.Equal(starts, []time.Duration{0, 12 * time.Second}) {
			t.Fatal("首轮失败后未按时继续", starts)
		}

		// 2. 核对结构化统计、统一错误码、完整任务 ID 和日志脱敏。
		type event struct {
			Message         string `json:"msg"`
			Level           string `json:"level"`
			IntervalMS      int    `json:"interval_ms"`
			Limit           int    `json:"batch_limit"`
			TaskID          uint64 `json:"task_id"`
			Code            string `json:"error_code"`
			Scanned         int    `json:"scanned"`
			Published       int    `json:"published"`
			Retries         int    `json:"retries_scheduled"`
			CandidateErrors int    `json:"candidate_errors"`
		}
		var events []event
		logged := logs.String()
		for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
			var entry event
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatal(err)
			}
			events = append(events, entry)
		}
		want := []event{
			{Message: "分片校验 worker 已启动", Level: "INFO", IntervalMS: 5000, Limit: 10},
			{Message: "分片校验轮次失败", Level: "WARN"},
			{Message: "分片校验任务处理失败", Level: "WARN", TaskID: 19, Code: "CHECKSUM_MISMATCH"},
			{Message: "分片校验任务处理失败", Level: "WARN", TaskID: math.MaxUint64, Code: "INTERNAL_ERROR"},
			{Message: "分片校验轮次结束", Level: "INFO", Scanned: 4, Published: 1, Retries: 1, CandidateErrors: 2},
			{Message: "分片校验 worker 已停止", Level: "INFO"},
		}
		if !slices.Equal(events, want) || strings.Contains(logged, privateDetail) {
			t.Fatal("分片校验轮次日志或脱敏结果错误", logged)
		}

		// 3. 新的运行实例获得独立租约标识。
		owners := make(chan string, 1)
		stopAgain := startServerMultipartUploadVerificationWorker(t.Context(), func(_ context.Context, _ int, owner string) (service.UploadVerificationBatchResult, error) {
			owners <- owner
			return service.UploadVerificationBatchResult{}, nil
		}, logging.New(io.Discard))
		secondOwner := <-owners
		stopAgain()
		if secondOwner == firstOwner || !strings.HasPrefix(secondOwner, ownerPrefix) {
			t.Fatal("两次运行共用租约持有者", firstOwner, secondOwner)
		}
	})
}

func TestStopServerMultipartUploadVerificationWorkerWaitsForActiveRound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 模拟一轮在收到取消后仍需完成资源清理。
		started, canceled, finishRound, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var logs bytes.Buffer
		stop := startServerMultipartUploadVerificationWorker(t.Context(), func(ctx context.Context, _ int, _ string) (service.UploadVerificationBatchResult, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-finishRound
			return service.UploadVerificationBatchResult{
				Scanned: 3, Published: 1,
				CandidateErrors: []service.UploadVerificationCandidateError{
					{TaskID: 19, Err: ctx.Err()},
					{TaskID: 20, Err: domain.ErrUploadTaskBusy},
				},
			}, ctx.Err()
		}, logging.New(&logs))
		<-started
		go func() {
			stop()
			close(stopped)
		}()
		<-canceled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("校验轮次尚未清理完成，停止方法已经返回")
		default:
		}
		close(finishRound)
		<-stopped
		stop()

		// 2. 停止返回前交付部分结果和独立任务错误。
		logged := logs.String()
		report := strings.Index(logged, "分片校验轮次结束")
		exit := strings.Index(logged, "分片校验 worker 已停止")
		if report < 0 || exit <= report || strings.Contains(logged, "分片校验轮次失败") ||
			!strings.Contains(logged, `"published":1`) || strings.Count(logged, "分片校验任务处理失败") != 1 ||
			!strings.Contains(logged, `"task_id":20`) || !strings.Contains(logged, `"error_code":"UPLOAD_BUSY"`) {
			t.Fatal("关停日志顺序、部分结果或取消过滤错误", logged)
		}
	})
}
