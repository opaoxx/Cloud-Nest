package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadContentLeaseRenewalSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		receiveContext, cancelReceive := context.WithCancelCause(t.Context())
		defer cancelReceive(nil)
		startedAt := time.Now()
		var renewals []time.Duration
		stop := startUploadContentLeaseRenewal(receiveContext, cancelReceive, func(ctx context.Context) error {
			if ctx != receiveContext || ctx.Err() != nil {
				t.Error("续租未使用本次接收的有效上下文")
			}
			renewals = append(renewals, time.Since(startedAt))
			return nil
		})
		defer stop()

		time.Sleep(20*time.Second - time.Nanosecond)
		synctest.Wait()
		if len(renewals) != 0 {
			t.Fatal("未满二十秒就开始续租", renewals)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !reflect.DeepEqual(renewals, []time.Duration{20 * time.Second}) {
			t.Fatal("首次续租时间错误", renewals)
		}
		time.Sleep(20 * time.Second)
		synctest.Wait()
		if err := stop(); err != nil || receiveContext.Err() != nil {
			t.Fatal("正常停止应保留接收上下文", err)
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if !reflect.DeepEqual(renewals, []time.Duration{20 * time.Second, 40 * time.Second}) {
			t.Fatal("续租间隔错误或停止后仍发起了续租", renewals)
		}
	})
}

func TestUploadContentLeaseRenewalFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		receiveContext, cancelReceive := context.WithCancelCause(t.Context())
		defer cancelReceive(nil)
		calls := 0
		stop := startUploadContentLeaseRenewal(receiveContext, cancelReceive, func(context.Context) error {
			calls++
			return domain.ErrUploadTaskBusy
		})
		defer stop()

		time.Sleep(20 * time.Second)
		synctest.Wait()
		if receiveContext.Err() != context.Canceled || context.Cause(receiveContext) != domain.ErrUploadTaskBusy {
			t.Fatal("续租失败未取消接收或丢失了原始错误", context.Cause(receiveContext))
		}
		time.Sleep(time.Minute)
		if err := stop(); err != domain.ErrUploadTaskBusy || calls != 1 {
			t.Fatal("失败后仍调用续租或丢失错误", err, calls)
		}
	})
}

func TestUploadContentLeaseRenewalStopWaits(t *testing.T) {
	for _, fixture := range []struct {
		name       string
		renewError error
	}{
		{"active renewal succeeds", nil},
		{"active renewal fails", domain.ErrUploadTaskBusy},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				receiveContext, cancelReceive := context.WithCancelCause(t.Context())
				defer cancelReceive(nil)
				releaseRenewal := make(chan struct{})
				calls := 0
				stop := startUploadContentLeaseRenewal(receiveContext, cancelReceive, func(ctx context.Context) error {
					calls++
					select {
					case <-releaseRenewal:
						return fixture.renewError
					case <-ctx.Done():
						return ctx.Err()
					}
				})
				time.Sleep(20 * time.Second)
				synctest.Wait()
				if calls != 1 {
					t.Fatal("停止前应有且仅有一次在途续租", calls)
				}
				stopped := make(chan error, 4)
				for range cap(stopped) {
					go func() { stopped <- stop() }()
				}
				time.Sleep(40 * time.Second)
				synctest.Wait()
				if len(stopped) != 0 || calls != 1 || receiveContext.Err() != nil {
					t.Fatal("续租未结束时提前停止或取消了请求")
				}
				close(releaseRenewal)
				synctest.Wait()
				if len(stopped) != cap(stopped) {
					t.Fatal("在途续租结束后仍有停止调用在等待")
				}
				for range cap(stopped) {
					if err := <-stopped; err != fixture.renewError {
						t.Fatal("停止结果未反映在途事务的结局", err)
					}
				}
				if calls != 1 || context.Cause(receiveContext) != fixture.renewError {
					t.Fatal("停止后仍续租或接收取消原因错误", calls, context.Cause(receiveContext))
				}
			})
		})
	}
}

func TestUploadContentLeaseRenewalParentCancellation(t *testing.T) {
	parentError := errors.New("fixture request canceled")
	for _, fixture := range []struct {
		name      string
		cancelAt  time.Duration
		wantCalls int
	}{
		{"before starting", 0, 0},
		{"while waiting", 10 * time.Second, 0},
		{"during renewal", 21 * time.Second, 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, cancelParent := context.WithCancelCause(t.Context())
				defer cancelParent(nil)
				receiveContext, cancelReceive := context.WithCancelCause(parent)
				defer cancelReceive(nil)
				if fixture.cancelAt == 0 {
					cancelParent(parentError)
				}
				calls := 0
				stop := startUploadContentLeaseRenewal(receiveContext, cancelReceive, func(ctx context.Context) error {
					calls++
					<-ctx.Done()
					return ctx.Err()
				})
				if fixture.cancelAt > 0 {
					time.Sleep(fixture.cancelAt)
					cancelParent(parentError)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if err := stop(); err != parentError || calls != fixture.wantCalls {
					t.Fatal("父请求取消未停止循环或原始原因丢失", err, calls)
				}
			})
		})
	}
}
