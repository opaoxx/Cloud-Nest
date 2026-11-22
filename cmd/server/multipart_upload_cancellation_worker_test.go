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
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerMultipartUploadCancellationWorkerReportsRounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const privateDetail = "fixture://private/staged/object?session=hidden"
		var logs bytes.Buffer
		var firstOwner string
		startedAt := time.Now()
		rounds := 0

		// 1. 首轮立即执行，扫描失败后下一轮沿用同一租约标识和扫描上限。
		stop := startServerMultipartUploadCancellationWorker(t.Context(), func(_ context.Context, limit int, owner string) (service.MultipartUploadCancellationBatchResult, error) {
			rounds++
			if limit != 100 {
				t.Error("单轮扫描上限不符", limit)
			}
			if rounds == 1 {
				firstOwner = owner
				if !time.Now().Equal(startedAt) || len(owner) > 64 || !strings.HasPrefix(owner, "multipart-cancel-") {
					t.Error("首轮开始时间或租约持有者不符", owner, time.Since(startedAt))
				}
				if _, err := uuid.Parse(strings.TrimPrefix(owner, "multipart-cancel-")); err != nil {
					t.Error("租约持有者缺少独立标识", err)
				}
				return service.MultipartUploadCancellationBatchResult{}, errors.New(privateDetail)
			}
			if owner != firstOwner || time.Since(startedAt) != 30*time.Second {
				t.Error("下一轮的租约持有者或开始时间不符", owner, time.Since(startedAt))
			}
			return service.MultipartUploadCancellationBatchResult{
				Scanned: 6, Cancelled: 2, RetriesScheduled: 1, Skipped: 1,
				CandidateErrors: []service.UploadCancellationCandidateError{
					{TaskID: 20, Err: fmt.Errorf("%s: %w", privateDetail, domain.ErrStoredObjectInUse)},
					{TaskID: math.MaxUint64, Err: errors.New(privateDetail)},
				},
			}, nil
		}, logging.New(&logs))
		synctest.Wait()
		time.Sleep(30*time.Second - time.Nanosecond)
		synctest.Wait()
		if rounds != 1 {
			t.Error("未等满间隔就开始下一轮", rounds)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		stop()

		// 2. 逐条比对 JSON 汇报，错误详情和租约标识不写入日志。
		type event struct {
			Message         string `json:"msg"`
			Code            string `json:"error_code"`
			TaskID          uint64 `json:"task_id"`
			Interval        int    `json:"interval_ms"`
			Limit           int    `json:"batch_limit"`
			Scanned         int    `json:"scanned"`
			Cancelled       int    `json:"cancelled"`
			Retries         int    `json:"retries_scheduled"`
			Skipped         int    `json:"skipped"`
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
			{Message: "分片上传取消 worker 已启动", Interval: 30000, Limit: 100},
			{Message: "分片上传取消轮次失败", Code: "INTERNAL_ERROR"},
			{Message: "分片上传取消任务处理失败", TaskID: 20, Code: "STORED_OBJECT_IN_USE"},
			{Message: "分片上传取消任务处理失败", TaskID: math.MaxUint64, Code: "INTERNAL_ERROR"},
			{Message: "分片上传取消轮次结束", Scanned: 6, Cancelled: 2, Retries: 1, Skipped: 1, CandidateErrors: 2},
			{Message: "分片上传取消 worker 已停止"},
		}
		if rounds != 2 || strings.Contains(logged, "hidden") || strings.Contains(logged, firstOwner) || !slices.Equal(events, want) {
			t.Fatal("轮次数、结构化日志或脱敏结果不符", rounds, logged)
		}

		// 3. 另一次启动使用不同的租约标识。
		owners := make(chan string, 1)
		stopAgain := startServerMultipartUploadCancellationWorker(t.Context(), func(_ context.Context, _ int, owner string) (service.MultipartUploadCancellationBatchResult, error) {
			owners <- owner
			return service.MultipartUploadCancellationBatchResult{}, nil
		}, logging.New(io.Discard))
		secondOwner := <-owners
		stopAgain()
		if secondOwner == "" || secondOwner == firstOwner {
			t.Fatal("两次 worker 启动共用了租约持有者")
		}
	})
}

func TestStopServerMultipartUploadCancellationWorkerWaitsForActiveRound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		cancelled := make(chan struct{})
		finishRound := make(chan struct{})
		stopped := make(chan struct{})
		var logs bytes.Buffer
		rounds := 0

		// 1. 模拟在途轮次先收到停止信号，再交付部分结果和独立故障。
		stop := startServerMultipartUploadCancellationWorker(t.Context(), func(ctx context.Context, _ int, _ string) (service.MultipartUploadCancellationBatchResult, error) {
			rounds++
			if rounds != 1 {
				t.Error("关停后开启了新轮次")
				return service.MultipartUploadCancellationBatchResult{}, ctx.Err()
			}
			close(started)
			<-ctx.Done()
			close(cancelled)
			<-finishRound
			stopError := fmt.Errorf("合成关停包装: %w", ctx.Err())
			return service.MultipartUploadCancellationBatchResult{
				Scanned: 5, Cancelled: 1, RetriesScheduled: 1,
				CandidateErrors: []service.UploadCancellationCandidateError{
					{TaskID: 19, Err: stopError},
					{TaskID: 20, Err: errors.Join(stopError, errors.New("fixture hidden rollback"))},
				},
			}, stopError
		}, logging.New(&logs))
		<-started
		go func() {
			stop()
			close(stopped)
		}()
		<-cancelled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("在途轮次尚未结束，停止方法已返回")
		default:
		}
		close(finishRound)
		<-stopped
		stop()
		time.Sleep(time.Minute)

		// 2. 停止返回前最后一轮已汇报，正常关停与真实故障分开处理。
		logged := logs.String()
		report := strings.Index(logged, "分片上传取消轮次结束")
		exit := strings.Index(logged, "分片上传取消 worker 已停止")
		if rounds != 1 || report < 0 || exit <= report || strings.Contains(logged, "分片上传取消轮次失败") ||
			strings.Contains(logged, "分片上传取消 worker 异常退出") {
			t.Fatal("停止顺序、轮次数或关停错误汇报不符", logged)
		}
		if strings.Count(logged, "分片上传取消任务处理失败") != 1 || strings.Contains(logged, `"task_id":19`) ||
			!strings.Contains(logged, `"task_id":20,"error_code":"INTERNAL_ERROR"`) || strings.Contains(logged, "hidden rollback") ||
			!strings.Contains(logged, `"cancelled":1,"retries_scheduled":1`) {
			t.Fatal("停止前未正确汇报已提交结果和独立故障", logged)
		}
	})
}
