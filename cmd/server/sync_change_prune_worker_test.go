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
)

type syncChangePruneWorkerStoreFixture struct {
	read  func(context.Context) (time.Time, error)
	list  func(context.Context, time.Time, uint64, int) (domain.SyncChangePruneCandidatePage, error)
	prune func(context.Context, uint64, time.Time, int) (domain.SyncChangePruneRange, error)
}

func (fixture syncChangePruneWorkerStoreFixture) ReadSyncChangePruneCutoff(ctx context.Context) (time.Time, error) {
	return fixture.read(ctx)
}

func (fixture syncChangePruneWorkerStoreFixture) ListSyncChangePruneCandidates(
	ctx context.Context, cutoff time.Time, afterUserID uint64, limit int,
) (domain.SyncChangePruneCandidatePage, error) {
	return fixture.list(ctx, cutoff, afterUserID, limit)
}

func (fixture syncChangePruneWorkerStoreFixture) PruneSyncChangeLogBatch(
	ctx context.Context, userID uint64, cutoff time.Time, limit int,
) (domain.SyncChangePruneRange, error) {
	return fixture.prune(ctx, userID, cutoff, limit)
}

func TestServerSyncChangePruneWorkerReportsAndWaitsForStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 首轮遇到时钟错误，第二轮用真实单轮服务处理替身仓储结果。
		var logs bytes.Buffer
		reachedLast, canceled := make(chan struct{}), make(chan struct{})
		release, stopped := make(chan struct{}), make(chan struct{})
		reads := 0
		start := time.Now()
		cutoff := time.Date(2026, time.August, 25, 12, 0, 0, 15625000, time.UTC)
		fixture := syncChangePruneWorkerStoreFixture{
			read: func(context.Context) (time.Time, error) {
				reads++
				if reads == 1 {
					if time.Since(start) != 0 {
						t.Error("首轮应立即开始")
					}
					return time.Time{}, errors.New("fixture private cutoff detail")
				}
				return cutoff, nil
			},
			list: func(_ context.Context, received time.Time, afterUserID uint64, limit int) (domain.SyncChangePruneCandidatePage, error) {
				if received != cutoff || limit != 50 {
					t.Errorf("分页截止点或上限不符：%v，%d", received, limit)
				}
				if afterUserID == 0 {
					return domain.SyncChangePruneCandidatePage{NextAfterUserID: 50, ScannedCount: 50}, nil
				}
				if afterUserID != 50 {
					t.Errorf("意外的分页位置：%d", afterUserID)
				}
				return domain.SyncChangePruneCandidatePage{UserIDs: []uint64{60, 61, 62, 63, 64, 65}, NextAfterUserID: 100, ScannedCount: 50}, nil
			},
			prune: func(roundContext context.Context, userID uint64, received time.Time, limit int) (domain.SyncChangePruneRange, error) {
				if received != cutoff || limit != 100 {
					t.Errorf("单账号截止点或上限不符：%v，%d", received, limit)
				}
				switch userID {
				case 60:
					return domain.SyncChangePruneRange{}, fmt.Errorf("fixture private count detail: %w", domain.ErrSyncChangePruneCountMismatch)
				case 61:
					return domain.SyncChangePruneRange{}, nil
				case 62:
					return domain.SyncChangePruneRange{FirstSequence: 18, LastSequence: 19, Count: 2}, nil
				case 63:
					return domain.SyncChangePruneRange{}, fmt.Errorf("fixture private sequence detail: %w", domain.ErrInvalidSyncSnapshotSequence)
				case 64:
					close(reachedLast)
					<-roundContext.Done()
					close(canceled)
					<-release
					time.Sleep(2 * time.Second)
					return domain.SyncChangePruneRange{},
						errors.Join(domain.ErrUserTransactionCommitUnconfirmed, roundContext.Err(), errors.New("fixture private commit detail"))
				default:
					t.Errorf("取消后仍处理账号：%d", userID)
					return domain.SyncChangePruneRange{}, nil
				}
			},
		}
		stop := startServerSyncChangePruneWorker(t.Context(), fixture, logging.New(&logs))
		<-reachedLast
		if time.Since(start) != time.Hour {
			t.Errorf("时钟查询失败后应等待完整一小时：%v", time.Since(start))
		}

		// 2. 关停发出取消，但在途任务未交付结果时不会返回。
		go func() {
			stop()
			close(stopped)
		}()
		<-canceled
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("在途裁剪尚未汇报，停止方法已结束")
		default:
		}
		close(release)
		<-stopped
		stop()

		// 3. 核对结构化日志字段、固定错误码和退出顺序。
		type workerLog struct {
			Message            string    `json:"msg"`
			Level              string    `json:"level"`
			IntervalMS         int64     `json:"interval_ms"`
			ScanLimit          int       `json:"scan_limit"`
			PageSize           int       `json:"page_size"`
			AccountDeleteLimit int       `json:"account_delete_limit"`
			DurationMS         int64     `json:"duration_ms"`
			Cutoff             time.Time `json:"cutoff"`
			Scanned            int       `json:"scanned"`
			PrunedAccounts     int       `json:"pruned_accounts"`
			DeletedEvents      int       `json:"deleted_events"`
			EmptyAccounts      int       `json:"empty_accounts"`
			AccountErrors      int       `json:"account_errors"`
			NextAfterUserID    uint64    `json:"next_after_user_id"`
			UserID             uint64    `json:"user_id"`
			ErrorCode          string    `json:"error_code"`
		}
		var actual []workerLog
		for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
			var entry workerLog
			if err := json.Unmarshal(line, &entry); err != nil {
				t.Fatal("解析裁剪日志失败", err)
			}
			actual = append(actual, entry)
		}
		want := []workerLog{
			{Message: "同步日志裁剪 worker 已启动", Level: "INFO", IntervalMS: 3600000, ScanLimit: 100, PageSize: 50, AccountDeleteLimit: 100},
			{Message: "同步日志裁剪轮次失败", Level: "WARN", ErrorCode: "INTERNAL_ERROR"},
			{Message: "同步日志裁剪轮次结束", Level: "INFO"},
			{Message: "同步日志裁剪账号失败", Level: "WARN", UserID: 60, ErrorCode: "SYNC_CHANGE_PRUNE_COUNT_MISMATCH"},
			{Message: "同步日志裁剪账号失败", Level: "WARN", UserID: 63, ErrorCode: "INVALID_SYNC_CHANGE_SEQUENCE"},
			{Message: "同步日志裁剪账号失败", Level: "WARN", UserID: 64, ErrorCode: "USER_TRANSACTION_COMMIT_UNCONFIRMED"},
			{Message: "同步日志裁剪轮次结束", Level: "INFO", DurationMS: 2000, Cutoff: cutoff, Scanned: 100,
				PrunedAccounts: 1, DeletedEvents: 2, EmptyAccounts: 1, AccountErrors: 3, NextAfterUserID: 64},
			{Message: "同步日志裁剪 worker 已停止", Level: "INFO"},
		}
		if reads != 2 || !reflect.DeepEqual(actual, want) || strings.Contains(logs.String(), "fixture private") {
			t.Fatalf("日志或退出顺序不符，读钟 %d 次：%s", reads, logs.String())
		}
	})
}

func TestServerSyncChangePruneWorkerWaitsForFinalReportOnServiceCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 1. 父上下文触发停止，在最后一轮汇报处暂停日志写入。
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var logs bytes.Buffer
		gate := &serverWorkerReportGate{
			Handler: logging.New(&logs).Handler(), reportMessage: "同步日志裁剪轮次结束",
			reportStarted: make(chan struct{}), releaseReport: make(chan struct{}),
		}
		started, finished := make(chan struct{}), make(chan struct{})
		cutoff := time.Date(2026, time.August, 25, 12, 0, 0, 15625000, time.UTC)
		fixture := syncChangePruneWorkerStoreFixture{
			read: func(context.Context) (time.Time, error) { return cutoff, nil },
			list: func(context.Context, time.Time, uint64, int) (domain.SyncChangePruneCandidatePage, error) {
				return domain.SyncChangePruneCandidatePage{UserIDs: []uint64{12}, NextAfterUserID: 12, ScannedCount: 1}, nil
			},
			prune: func(roundContext context.Context, _ uint64, _ time.Time, _ int) (domain.SyncChangePruneRange, error) {
				close(started)
				<-roundContext.Done()
				return domain.SyncChangePruneRange{FirstSequence: 18, LastSequence: 18, Count: 1}, nil
			},
		}
		stop := startServerSyncChangePruneWorker(ctx, fixture, slog.New(gate))
		<-started
		cancel()
		<-gate.reportStarted

		// 2. 确认汇报写完之后，停止函数才返回。
		go func() {
			stop()
			close(finished)
		}()
		synctest.Wait()
		select {
		case <-finished:
			t.Error("汇报尚未写完，停止方法已结束")
		default:
		}
		close(gate.releaseReport)
		<-finished
		logText := logs.String()
		report := strings.Index(logText, "同步日志裁剪轮次结束")
		stopped := strings.Index(logText, "同步日志裁剪 worker 已停止")
		if report < 0 || stopped <= report || !strings.Contains(logText, `"deleted_events":1`) || strings.Contains(logText, `"level":"WARN"`) {
			t.Fatalf("父上下文取消后的汇报或退出顺序不符：%s", logText)
		}
	})
}
