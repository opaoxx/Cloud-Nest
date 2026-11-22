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

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestServerMultipartUploadExpirationWorkerReportsRounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const privateDetail = "fixture://private/object?session=hidden"
		const ownerPrefix = "multipart-expire-"
		var logs bytes.Buffer
		var owners []string
		start := time.Now()
		var starts []time.Duration
		var rounds atomic.Int32

		// 1. 首轮返回扫描错误，第二轮返回完整计数和逐项错误。
		stop := startServerMultipartUploadExpirationWorker(t.Context(), func(_ context.Context, limit int, owner string) (service.MultipartUploadExpirationBatchResult, error) {
			owners = append(owners, owner)
			starts = append(starts, time.Since(start))
			if limit != 100 {
				t.Error("单轮扫描上限不符", limit)
			}
			if rounds.Add(1) == 1 {
				return service.MultipartUploadExpirationBatchResult{}, errors.New(privateDetail)
			}
			return service.MultipartUploadExpirationBatchResult{
				Scanned: 6, Expired: 2, RetriesScheduled: 1, Skipped: 1,
				CandidateErrors: []service.UploadExpirationCandidateError{
					{TaskID: 20, Err: fmt.Errorf("%s: %w", privateDetail, domain.ErrStoredObjectInUse)},
					{TaskID: math.MaxUint64, Err: errors.New(privateDetail)},
				},
			}, nil
		}, logging.New(&logs))
		synctest.Wait()
		time.Sleep(30*time.Second - time.Nanosecond)
		synctest.Wait()
		if count := rounds.Load(); count != 1 {
			t.Error("未等待完整间隔", count)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		stop()
		if !slices.Equal(starts, []time.Duration{0, 30 * time.Second}) || len(owners) != 2 || owners[0] != owners[1] ||
			!strings.HasPrefix(owners[0], ownerPrefix) || len(owners[0]) > 64 {
			t.Fatal("轮间隔或本次运行的租约持有者不符", starts, owners)
		}
		if _, err := uuid.Parse(strings.TrimPrefix(owners[0], ownerPrefix)); err != nil {
			t.Fatal("租约持有者缺少独立 UUID", err)
		}

		// 2. 解码完整 JSON，核对日志保留任务 ID 和固定错误码。
		type event struct {
			Message  string `json:"msg"`
			Code     string `json:"error_code"`
			TaskID   uint64 `json:"task_id"`
			Interval int    `json:"interval_ms"`
			Limit    int    `json:"batch_limit"`
			Scanned  int    `json:"scanned"`
			Expired  int    `json:"expired"`
			Retries  int    `json:"retries_scheduled"`
			Skipped  int    `json:"skipped"`
			Failures int    `json:"candidate_errors"`
		}
		var events []event
		decoder := json.NewDecoder(&logs)
		logged := logs.String()
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
			{Message: "分片上传到期 worker 已启动", Interval: 30000, Limit: 100},
			{Message: "分片上传到期轮次失败", Code: "INTERNAL_ERROR"},
			{Message: "分片上传到期任务处理失败", TaskID: 20, Code: "STORED_OBJECT_IN_USE"},
			{Message: "分片上传到期任务处理失败", TaskID: math.MaxUint64, Code: "INTERNAL_ERROR"},
			{Message: "分片上传到期轮次结束", Scanned: 6, Expired: 2, Retries: 1, Skipped: 1, Failures: 2},
			{Message: "分片上传到期 worker 已停止"},
		}
		if !slices.Equal(events, want) || strings.Contains(logged, privateDetail) || strings.Contains(logged, owners[0]) || strings.Contains(logged, `"error":`) {
			t.Fatal("轮次汇报或日志字段不符", logged)
		}

		// 3. 再启动一次，领取使用独立标识。
		otherOwner := make(chan string, 1)
		stopAgain := startServerMultipartUploadExpirationWorker(t.Context(), func(_ context.Context, _ int, owner string) (service.MultipartUploadExpirationBatchResult, error) {
			otherOwner <- owner
			return service.MultipartUploadExpirationBatchResult{}, nil
		}, logging.New(io.Discard))
		newOwner := <-otherOwner
		stopAgain()
		if newOwner == "" || newOwner == owners[0] {
			t.Fatal("不同启动共用了同一个租约持有者")
		}
	})
}

func TestStopServerMultipartUploadExpirationWorkerWaitsForActiveRound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		canceled, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var logs bytes.Buffer
		rounds := 0
		stop := startServerMultipartUploadExpirationWorker(t.Context(), func(ctx context.Context, _ int, _ string) (service.MultipartUploadExpirationBatchResult, error) {
			rounds++
			<-ctx.Done()
			close(canceled)
			<-release
			stopError := fmt.Errorf("合成关停: %w", ctx.Err())
			return service.MultipartUploadExpirationBatchResult{Scanned: 4, Expired: 1, RetriesScheduled: 1,
				CandidateErrors: []service.UploadExpirationCandidateError{
					{TaskID: 19, Err: stopError},
					{TaskID: 20, Err: errors.Join(stopError, errors.New("fixture hidden rollback"))},
				}}, stopError
		}, logging.New(&logs))
		synctest.Wait()
		go func() {
			stop()
			close(stopped)
		}()
		<-canceled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("在途轮次尚未释放，停止方法已返回")
		default:
		}
		close(release)
		<-stopped
		stop()
		time.Sleep(time.Minute)

		// 1. 停止返回前交付部分结果及独立故障，省略纯关停告警。
		logged := logs.String()
		report := strings.Index(logged, "分片上传到期轮次结束")
		exit := strings.Index(logged, "分片上传到期 worker 已停止")
		if rounds != 1 || report < 0 || exit <= report || strings.Contains(logged, "分片上传到期轮次失败") ||
			strings.Contains(logged, "分片上传到期 worker 异常退出") || strings.Contains(logged, `"task_id":19`) ||
			!strings.Contains(logged, `"task_id":20,"error_code":"INTERNAL_ERROR"`) || strings.Contains(logged, "hidden rollback") ||
			!strings.Contains(logged, `"expired":1,"retries_scheduled":1,"skipped":0,"candidate_errors":2`) {
			t.Fatal("停止顺序、部分汇总或错误日志不符", logged)
		}
	})
}
