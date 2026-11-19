package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerUploadCancellationWorkerReportsFailureAndWaitsForStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 注入一次扫描失败，让下一轮停在清理过程中。
		started := make(chan time.Time, 2)
		canceled := make(chan struct{})
		finish := make(chan struct{})
		stopped := make(chan struct{})
		var logs bytes.Buffer
		const privateError = "fixture-private-database-error"
		startedAt := time.Now()
		rounds := 0
		stop := startServerUploadCancellationWorker(t.Context(), func(ctx context.Context, limit int) (service.UploadCancellationBatchResult, error) {
			if limit != 100 {
				t.Error("取消批量上限不符", limit)
			}
			rounds++
			started <- time.Now()
			if rounds == 1 {
				return service.UploadCancellationBatchResult{}, errors.New(privateError)
			}
			<-ctx.Done()
			close(canceled)
			<-finish
			return service.UploadCancellationBatchResult{
				Scanned: 3, Cancelled: 1,
				CandidateErrors: []service.UploadCancellationCandidateError{
					{TaskID: 19, Err: domain.ErrUploadTaskBusy},
					{TaskID: 20, Err: fmt.Errorf("收尾停止: %w", ctx.Err())},
				},
			}, ctx.Err()
		}, logging.New(&logs))

		// 2. 首轮立即开始，扫描失败后按 30 秒间隔继续。
		if first, second := <-started, <-started; !first.Equal(startedAt) || second.Sub(first) != 30*time.Second {
			t.Error("首轮与下一轮的执行时间不符", first.Sub(startedAt), second.Sub(first))
		}
		go func() {
			stop()
			close(stopped)
		}()
		<-canceled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("当前轮次尚未退出，停止方法已经返回")
		default:
		}

		// 3. 清理完成后关闭 worker，核对故障和关停时的汇报。
		close(finish)
		<-stopped
		stop()
		logged := logs.String()
		failure := strings.Index(logged, "上传取消任务处理失败")
		report := strings.Index(logged, "上传取消轮次结束")
		exit := strings.Index(logged, "上传取消 worker 已停止")
		if strings.Count(logged, "上传取消轮次失败") != 1 || strings.Contains(logged, privateError) ||
			failure < 0 || report <= failure || exit <= report || !strings.Contains(logged, `"scanned":3,"cancelled":1,"retries_scheduled":0,"candidate_errors":2`) {
			t.Fatal("取消 worker 的故障或关停汇报不符", logged)
		}
		if strings.Count(logged, "上传取消任务处理失败") != 1 ||
			!strings.Contains(logged[failure:report], `"task_id":19,"error_code":"UPLOAD_BUSY"`) ||
			strings.Contains(logged, `"task_id":20`) {
			t.Fatal("关停轮次应汇报独立故障并跳过停止警告", logged)
		}
	})
}

func TestServerUploadCancellationWorkerLogsCandidateFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 构造服务正常运行时的一轮结果，覆盖已知和未知原因。
		const privateDetail = "fixture-private-upload-detail"
		var logs bytes.Buffer
		const candidateWarning = "上传取消任务处理失败"
		stop := startServerUploadCancellationWorker(t.Context(), func(context.Context, int) (service.UploadCancellationBatchResult, error) {
			return service.UploadCancellationBatchResult{
				Scanned: 6, Cancelled: 1, RetriesScheduled: 1,
				CandidateErrors: []service.UploadCancellationCandidateError{
					{TaskID: 17, Err: fmt.Errorf("%s: %w", privateDetail, domain.ErrStoredObjectInUse)},
					{TaskID: 18, Err: domain.ErrUploadTaskNotReady},
					{TaskID: 19, Err: context.Canceled},
					{TaskID: math.MaxUint64, Err: errors.New(privateDetail)},
				},
			}, nil
		}, logging.New(&logs))
		synctest.Wait()
		stop()

		// 2. 读取结构化日志，核对顺序、级别、错误码和完整的无符号任务 ID。
		type failureLog struct {
			Level  string `json:"level"`
			TaskID uint64 `json:"task_id"`
			Code   string `json:"error_code"`
		}
		logged := logs.String()
		var warnings []failureLog
		for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
			if !strings.Contains(line, candidateWarning) {
				continue
			}
			var warning failureLog
			if err := json.Unmarshal([]byte(line), &warning); err != nil {
				t.Fatal(err)
			}
			warnings = append(warnings, warning)
		}
		want := []failureLog{
			{Level: "WARN", TaskID: 17, Code: "STORED_OBJECT_IN_USE"},
			{Level: "WARN", TaskID: 18, Code: "TASK_NOT_READY"},
			{Level: "WARN", TaskID: 19, Code: "CANCELED"},
			{Level: "WARN", TaskID: math.MaxUint64, Code: "INTERNAL_ERROR"},
		}
		if !slices.Equal(warnings, want) || strings.Contains(logged, privateDetail) ||
			strings.Contains(logged, `"error":`) || !strings.Contains(logged, `"scanned":6,"cancelled":1,"retries_scheduled":1,"candidate_errors":4`) {
			t.Fatal("逐条警告或轮次汇报不符", warnings, logged)
		}
	})
}
