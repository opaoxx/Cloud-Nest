package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
)

func TestRunMultipartUploadSessionCleanupLoopCarriesPositionBetweenRounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const interval = 30 * time.Second
		target, sessions := sessionCandidateFixture("pending-session")
		pending := MultipartUploadSessionCleanupPosition{
			cursor:  MultipartUploadSessionCandidateCursor{ObjectID: target.ObjectID, UploadID: "page-marker"},
			pending: []multipartUploadSessionCleanupCandidate{{target: target, session: sessions[0]}},
		}
		fallback := MultipartUploadSessionCleanupPosition{cursor: MultipartUploadSessionCandidateCursor{ObjectID: target.ObjectID - 1}}
		end := MultipartUploadSessionCleanupPosition{cursor: MultipartUploadSessionCandidateCursor{ObjectID: target.ObjectID}, atEnd: true}
		scanFailure := errors.New("fixture scan failure")
		steps := []struct {
			position MultipartUploadSessionCleanupPosition
			summary  MultipartUploadSessionCleanupRoundResult
			err      error
			second   int
		}{
			{summary: MultipartUploadSessionCleanupRoundResult{Next: pending, Confirmed: 1}},
			{position: pending, summary: MultipartUploadSessionCleanupRoundResult{Next: pending}, err: scanFailure, second: 67},
			{position: pending, summary: MultipartUploadSessionCleanupRoundResult{Next: fallback, ScanRestarted: true}, second: 97},
			{position: fallback, summary: MultipartUploadSessionCleanupRoundResult{Next: end, Done: true}, second: 127},
			{second: 157},
		}
		start := time.Now()
		executed, reported := 0, 0

		// 1. 首轮从空位置开始，经过续扫、错误、回退和完整扫描后再次从头开始。
		err := RunMultipartUploadSessionCleanupLoop(ctx, interval, func(roundContext context.Context, position MultipartUploadSessionCleanupPosition) (MultipartUploadSessionCleanupRoundResult, error) {
			if executed >= len(steps) || executed != reported {
				t.Fatal("轮次重叠或未等到上一轮汇报", executed, reported)
			}
			step := steps[executed]
			if roundContext != ctx || !reflect.DeepEqual(position, step.position) || time.Since(start) != time.Duration(step.second)*time.Second {
				t.Fatal("续扫位置、上下文或时间不符", executed, position, time.Since(start))
			}
			executed++
			if executed == 1 {
				time.Sleep(35 * time.Second)
			}
			return step.summary, step.err
		}, func(summary MultipartUploadSessionCleanupRoundResult, roundError error) {
			step := steps[reported]
			if !reflect.DeepEqual(summary, step.summary) || !errors.Is(roundError, step.err) {
				t.Fatal("汇报未保留本轮结果和错误", reported, summary, roundError)
			}
			reported++
			if reported == 1 {
				if time.Since(start) != 35*time.Second {
					t.Fatal("首轮执行结束前开始汇报")
				}
				time.Sleep(2 * time.Second)
			}
			if reported == len(steps) {
				cancel()
			}
		})
		if !errors.Is(err, context.Canceled) || executed != len(steps) || reported != executed {
			t.Fatal("退出原因、执行或汇报次数不符", executed, reported, err)
		}
	})
}

func TestRunMultipartUploadSessionCleanupLoopReportsBeforeStopping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		target, sessions := sessionCandidateFixture("interrupted-session")
		failure := errors.New("fixture candidate observation failed")
		partial := MultipartUploadSessionCleanupRoundResult{
			Next:   MultipartUploadSessionCleanupPosition{pending: []multipartUploadSessionCleanupCandidate{{target: target, session: sessions[0]}}},
			Items:  []MultipartUploadSessionCleanupItem{{Target: target, Session: sessions[0], Err: failure}},
			Failed: 1,
		}
		var rounds, reports int
		err := RunMultipartUploadSessionCleanupLoop(ctx, 30*time.Second, func(roundContext context.Context, _ MultipartUploadSessionCleanupPosition) (MultipartUploadSessionCleanupRoundResult, error) {
			rounds++
			<-roundContext.Done()
			return partial, roundContext.Err()
		}, func(summary MultipartUploadSessionCleanupRoundResult, err error) {
			reports++
			if !reflect.DeepEqual(summary, partial) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("关停前未汇报完整的部分结果", summary, err)
			}
		})
		if !errors.Is(err, context.DeadlineExceeded) || rounds != 1 || reports != 1 {
			t.Fatal("取消后启动了额外轮次或未传递期限原因", rounds, reports, err)
		}
	})
}

func TestRunMultipartUploadSessionCleanupLoopStopsWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		start := time.Now()
		rounds, reports := 0, 0
		finished := make(chan error, 1)
		go func() {
			finished <- RunMultipartUploadSessionCleanupLoop(ctx, 30*time.Second,
				func(context.Context, MultipartUploadSessionCleanupPosition) (MultipartUploadSessionCleanupRoundResult, error) {
					rounds++
					return MultipartUploadSessionCleanupRoundResult{}, nil
				}, func(MultipartUploadSessionCleanupRoundResult, error) { reports++ })
		}()
		synctest.Wait()
		time.Sleep(time.Second)
		cancel()
		if err := <-finished; !errors.Is(err, context.Canceled) || rounds != 1 || reports != 1 || time.Since(start) != time.Second {
			t.Fatal("轮间取消后仍继续等待或开始新轮", rounds, reports, time.Since(start), err)
		}
	})
}

func TestRunMultipartUploadSessionCleanupLoopRejectsInvalidInputs(t *testing.T) {
	runOnce := func(context.Context, MultipartUploadSessionCleanupPosition) (MultipartUploadSessionCleanupRoundResult, error) {
		t.Fatal("无效调用执行了首轮")
		return MultipartUploadSessionCleanupRoundResult{}, nil
	}
	report := func(MultipartUploadSessionCleanupRoundResult, error) { t.Fatal("无效调用交付了汇报") }
	for _, err := range []error{
		RunMultipartUploadSessionCleanupLoop(nil, time.Second, runOnce, report),
		RunMultipartUploadSessionCleanupLoop(t.Context(), 0, runOnce, report),
		RunMultipartUploadSessionCleanupLoop(t.Context(), time.Second, nil, report),
		RunMultipartUploadSessionCleanupLoop(t.Context(), time.Second, runOnce, nil),
	} {
		if err == nil {
			t.Fatal("无效参数通过验证")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := RunMultipartUploadSessionCleanupLoop(ctx, time.Second, runOnce, report); !errors.Is(err, context.Canceled) {
		t.Fatal("预先取消时未返回原因", err)
	}
}
