package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestHealthServiceReadyChecks(t *testing.T) {
	// 1. 覆盖成功和各项故障，验证实际调用顺序与提前返回规则。
	testCases := []struct {
		name          string
		databaseError bool
		storageError  bool
		wantReady     bool
		wantOrder     []string
	}{
		{name: "all healthy", wantReady: true, wantOrder: []string{"database", "storage"}},
		{name: "database unavailable", databaseError: true, wantOrder: []string{"database"}},
		{name: "storage unavailable", storageError: true, wantOrder: []string{"database", "storage"}},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var order []string
			var databaseContext context.Context
			healthService := NewHealthService(func(ctx context.Context) error {
				order = append(order, "database")
				databaseContext = ctx
				deadline, exists := ctx.Deadline()
				if !exists || time.Until(deadline) > 5*time.Second {
					t.Error("数据库检查没有正确的期限")
				}
				if testCase.databaseError {
					return errors.New("private database error")
				}
				return nil
			}, func(ctx context.Context) error {
				order = append(order, "storage")
				if ctx != databaseContext {
					t.Error("两个检查没有共享同一个上下文")
				}
				if testCase.storageError {
					return errors.New("private storage error")
				}
				return nil
			})

			// 2. 构造器只保存能力；收到请求后才执行检查。
			if len(order) != 0 {
				t.Fatal("构造器提前执行了检查")
			}
			if ready := healthService.Ready(context.Background()); ready != testCase.wantReady || !reflect.DeepEqual(order, testCase.wantOrder) {
				t.Fatalf("ready=%t，调用顺序=%v", ready, order)
			}
		})
	}
}

func TestHealthServiceReadyDeadlines(t *testing.T) {
	// 1. 模拟一个遵守上下文但一直等待的数据库检查。
	for _, callerTimeout := range []time.Duration{100 * time.Millisecond, 10 * time.Second} {
		t.Run(callerTimeout.String(), func(t *testing.T) {
			storageCalled := false
			healthService := NewHealthService(func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			}, func(ctx context.Context) error {
				storageCalled = true
				return nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), callerTimeout)
			defer cancel()
			startedAt := time.Now()
			ready := healthService.Ready(ctx)
			elapsed := time.Since(startedAt)

			// 2. 故障后不再检查存储，调用方短期限和默认总期限都必须生效。
			if ready || storageCalled {
				t.Fatal("超时后仍报告就绪或继续检查存储")
			}
			if callerTimeout == 100*time.Millisecond && elapsed > 2*time.Second {
				t.Fatalf("未及时响应短期限，耗时 %s", elapsed)
			}
			if callerTimeout == 10*time.Second && (elapsed < 4*time.Second || elapsed > 8*time.Second) {
				t.Fatalf("共享 5 秒期限未生效，耗时 %s", elapsed)
			}
		})
	}
}

func TestHealthServiceReadyCancellation(t *testing.T) {
	// 1. 检查取消发生在调用前或任一检查内，即使检查返回 nil 也不能报告就绪。
	for _, cancellationStage := range []string{"before", "database", "storage"} {
		t.Run(cancellationStage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			checkCount := 0
			if cancellationStage == "before" {
				cancel()
			}
			healthService := NewHealthService(func(ctx context.Context) error {
				checkCount++
				if cancellationStage == "database" {
					cancel()
				}
				return nil
			}, func(ctx context.Context) error {
				checkCount++
				cancel()
				return nil
			})
			if healthService.Ready(ctx) {
				t.Fatal("已取消的检查仍报告就绪")
			}
			wantCount := map[string]int{"before": 0, "database": 1, "storage": 2}[cancellationStage]
			if checkCount != wantCount {
				t.Fatalf("检查次数=%d，预期=%d", checkCount, wantCount)
			}
		})
	}
}
