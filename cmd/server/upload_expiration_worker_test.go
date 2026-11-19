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
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerUploadExpirationWorkerScheduleAndStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 第一轮完成，第二轮收到取消后还要等待清理结束。
		started := make(chan struct{})
		canceled := make(chan struct{})
		released := make(chan struct{})
		stopped := make(chan struct{})
		var logs bytes.Buffer
		var rounds atomic.Int32
		stop := startServerUploadExpirationWorker(t.Context(), func(ctx context.Context, limit int) (service.UploadExpirationBatchResult, error) {
			if limit != 100 {
				t.Error("单文件过期批量上限错误", limit)
			}
			if rounds.Add(1) == 1 {
				return service.UploadExpirationBatchResult{Scanned: 2, Expired: 1, RetriesScheduled: 1}, nil
			}
			close(started)
			<-ctx.Done()
			close(canceled)
			<-released
			return service.UploadExpirationBatchResult{
				Scanned: 2,
				CandidateErrors: []service.UploadExpirationCandidateError{
					{TaskID: 19, Err: domain.ErrUploadTaskBusy},
					{TaskID: 20, Err: fmt.Errorf("收尾事务: %w", ctx.Err())},
				},
			}, ctx.Err()
		}, logging.New(&logs))

		// 2. 用虚拟时钟核对首轮立即开始及后续间隔。
		synctest.Wait()
		if count := rounds.Load(); count != 1 {
			t.Error("首轮未立即执行", count)
		}
		time.Sleep(30*time.Second - time.Millisecond)
		synctest.Wait()
		if count := rounds.Load(); count != 1 {
			t.Error("过期处理在间隔结束前就开始下一轮", count)
		}
		time.Sleep(time.Millisecond)
		<-started

		// 3. 停止方法发出取消，并等待当前轮次退出。
		go func() {
			stop()
			close(stopped)
		}()
		<-canceled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("当前过期处理还未结束，停止方法已经返回")
		default:
		}
		close(released)
		<-stopped
		stop()
		logged := logs.String()
		report := strings.LastIndex(logged, "上传过期轮次结束")
		exit := strings.Index(logged, "上传过期 worker 已停止")
		if count := rounds.Load(); count != 2 || report < 0 || exit <= report ||
			!strings.Contains(logged, `"expired":1,"retries_scheduled":1`) || strings.Contains(logged, "上传过期轮次失败") {
			t.Fatal("过期汇报、关停顺序或取消日志不符", count, logged)
		}
		failure := strings.Index(logged, "上传过期任务处理失败")
		if failure < 0 || failure >= report || strings.Count(logged, "上传过期任务处理失败") != 1 ||
			!strings.Contains(logged, `"task_id":19,"error_code":"UPLOAD_BUSY"`) ||
			!strings.Contains(logged[report:], `"candidate_errors":2`) {
			t.Fatal("退出时应保留独立错误日志及本轮计数，略过取消警告", logged)
		}
	})
}

func TestServerUploadExpirationWorkerLogsCandidateFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 让首轮返回已知错误、未知错误和服务仍运行时的上下文错误。
		const privateDetail = "fixture://private/object?credential=hidden"
		var logs bytes.Buffer
		stop := startServerUploadExpirationWorker(t.Context(), func(context.Context, int) (service.UploadExpirationBatchResult, error) {
			return service.UploadExpirationBatchResult{
				Scanned: 4, Expired: 1,
				CandidateErrors: []service.UploadExpirationCandidateError{
					{TaskID: 17, Err: fmt.Errorf("%s: %w", privateDetail, domain.ErrStoredObjectInUse)},
					{TaskID: math.MaxUint64, Err: errors.New(privateDetail)},
					{TaskID: 18, Err: context.Canceled},
				},
			}, nil
		}, logging.New(&logs))
		synctest.Wait()
		stop()

		// 2. 解码 JSON，检查警告中的完整任务 ID、固定码和原始细节是否被过滤。
		type failureLog struct {
			Message string `json:"msg"`
			Level   string `json:"level"`
			TaskID  uint64 `json:"task_id"`
			Code    string `json:"error_code"`
		}
		logged := logs.String()
		var actual []failureLog
		for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
			var entry failureLog
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatal(err)
			}
			if entry.Message == "上传过期任务处理失败" {
				actual = append(actual, entry)
			}
		}
		want := []failureLog{
			{Message: "上传过期任务处理失败", Level: "WARN", TaskID: 17, Code: "STORED_OBJECT_IN_USE"},
			{Message: "上传过期任务处理失败", Level: "WARN", TaskID: math.MaxUint64, Code: "INTERNAL_ERROR"},
			{Message: "上传过期任务处理失败", Level: "WARN", TaskID: 18, Code: "CANCELED"},
		}
		if !slices.Equal(actual, want) || strings.Contains(logged, privateDetail) ||
			strings.Contains(logged, `"error":`) || !strings.Contains(logged, `"candidate_errors":3`) {
			t.Fatal("逐条错误字段、脱敏或汇总错误", actual, logged)
		}
	})
}
