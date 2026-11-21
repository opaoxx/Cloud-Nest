package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/logging"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestStopServerUploadMergeWorkerWaitsForActiveRound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		canceled := make(chan struct{})
		finishRound := make(chan struct{})
		stopped := make(chan struct{})
		var logs bytes.Buffer

		// 1. 让在途轮次在收到取消后继续收尾，阻止它立即返回。
		stop := startServerUploadMergeWorker(t.Context(), func(ctx context.Context, limit int, owner string) (service.UploadMergeBatchResult, error) {
			if limit != 10 || len(owner) > 64 || !strings.HasPrefix(owner, "multipart-merge-") {
				t.Error("worker 的扫描上限或租约持有者不符", limit, owner)
			}
			if _, err := uuid.Parse(strings.TrimPrefix(owner, "multipart-merge-")); err != nil {
				t.Error("租约持有者缺少独立标识", err)
			}
			close(started)
			<-ctx.Done()
			close(canceled)
			<-finishRound
			return service.UploadMergeBatchResult{
				Scanned: 3, Verifying: 1,
				CandidateErrors: []service.UploadMergeCandidateError{
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
			t.Error("在途轮次尚未完成，停止方法已返回")
		default:
		}
		close(finishRound)
		<-stopped
		stop()

		// 2. 停止方法返回前已汇报部分结果和独立故障。
		logged := logs.String()
		report := strings.Index(logged, "分片合并轮次结束")
		exit := strings.Index(logged, "分片合并 worker 已停止")
		if report < 0 || exit <= report || strings.Contains(logged, "分片合并轮次失败") {
			t.Fatal("停止顺序不符，或正常取消被记为轮次失败", logged)
		}
		if strings.Count(logged, "分片合并任务处理失败") != 1 || !strings.Contains(logged, `"task_id":20`) ||
			!strings.Contains(logged, `"error_code":"UPLOAD_BUSY"`) || !strings.Contains(logged, `"verifying":1`) {
			t.Fatal("停止前未汇报已提交结果及独立故障", logged)
		}
	})
}

func TestServerUploadMergeWorkerReportsRounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const privateDetail = "fixture://private/objects/hidden?session=hidden"
		var logs bytes.Buffer
		var firstOwner string
		var startedAt time.Time
		rounds := 0

		// 1. 首轮模拟扫描故障，其后检查新一轮沿用同一租约持有者。
		stop := startServerUploadMergeWorker(t.Context(), func(_ context.Context, limit int, owner string) (service.UploadMergeBatchResult, error) {
			rounds++
			if limit != 10 {
				t.Error("单轮扫描上限不符", limit)
			}
			if rounds == 1 {
				firstOwner, startedAt = owner, time.Now()
				return service.UploadMergeBatchResult{}, errors.New(privateDetail)
			}
			if owner != firstOwner || time.Since(startedAt) != 5*time.Second {
				t.Error("新一轮的租约持有者或启动间隔不符", time.Since(startedAt))
			}
			return service.UploadMergeBatchResult{
				Scanned: 7, Verifying: 1, Reopened: 1, Failed: 1, RetriesScheduled: 1, Skipped: 1,
				CandidateErrors: []service.UploadMergeCandidateError{
					{TaskID: 19, Err: fmt.Errorf("%s: %w", privateDetail, domain.ErrUploadStorageUnavailable)},
					{TaskID: math.MaxUint64, Err: errors.New(privateDetail)},
				},
			}, nil
		}, logging.New(&logs))
		synctest.Wait()
		time.Sleep(5 * time.Second)
		synctest.Wait()
		stop()

		// 2. 核对结构化日志顺序、完整任务 ID 和稳定错误码。
		type event struct {
			Message         string `json:"msg"`
			Code            string `json:"error_code"`
			TaskID          uint64 `json:"task_id"`
			Interval        int    `json:"interval_ms"`
			Limit           int    `json:"batch_limit"`
			Scanned         int    `json:"scanned"`
			Verifying       int    `json:"verifying"`
			Reopened        int    `json:"reopened"`
			Failed          int    `json:"failed"`
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
			{Message: "分片合并 worker 已启动", Interval: 5000, Limit: 10},
			{Message: "分片合并轮次失败", Code: "INTERNAL_ERROR"},
			{Message: "分片合并任务处理失败", TaskID: 19, Code: "STORAGE_UNAVAILABLE"},
			{Message: "分片合并任务处理失败", TaskID: math.MaxUint64, Code: "INTERNAL_ERROR"},
			{Message: "分片合并轮次结束", Scanned: 7, Verifying: 1, Reopened: 1, Failed: 1, Retries: 1, Skipped: 1, CandidateErrors: 2},
			{Message: "分片合并 worker 已停止"},
		}
		if rounds != 2 || strings.Contains(logged, privateDetail) || !reflect.DeepEqual(events, want) {
			t.Fatal("轮次数、结构化日志或脱敏结果不符", rounds, logged)
		}

		// 3. 再启动一次，检查不同运行拥有不同租约标识。
		owners := make(chan string, 1)
		stopAgain := startServerUploadMergeWorker(t.Context(), func(_ context.Context, _ int, owner string) (service.UploadMergeBatchResult, error) {
			owners <- owner
			return service.UploadMergeBatchResult{}, nil
		}, logging.New(io.Discard))
		secondOwner := <-owners
		stopAgain()
		if secondOwner == "" || secondOwner == firstOwner {
			t.Fatal("两次运行共用了租约标识")
		}
	})
}
