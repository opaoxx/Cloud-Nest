package binding

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"testing/synctest"
	"time"
)

type uploadDeadlineWriter struct {
	http.ResponseWriter
	deadlines []time.Time
	failure   error
}

func (writer *uploadDeadlineWriter) SetReadDeadline(deadline time.Time) error {
	writer.deadlines = append(writer.deadlines, deadline)
	return writer.failure
}

func TestWithUploadTransferDeadlinePolicy(t *testing.T) {
	for _, limit := range []time.Duration{time.Minute, 5 * time.Minute, 10 * time.Minute} {
		t.Run(limit.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, cancel := context.WithTimeout(t.Context(), limit)
				defer cancel()
				writer := &uploadDeadlineWriter{ResponseWriter: httptest.NewRecorder()}
				want := time.Now().Add(min(limit, 5*time.Minute))
				var child context.Context
				err := WithUploadTransferDeadline(parent, writer, func(ctx context.Context) error {
					child = ctx
					deadline, ok := ctx.Deadline()
					if !ok || !deadline.Equal(want) || len(writer.deadlines) != 1 || !writer.deadlines[0].Equal(want) || ctx.Err() != nil {
						t.Fatal("传输和正文读取没有共用正确期限", deadline, writer.deadlines)
					}
					return nil
				})
				if err != nil || child == nil || child.Err() != context.Canceled || parent.Err() != nil || len(writer.deadlines) != 1 {
					t.Fatal("传输退出未释放子上下文，或提前清除了读取期限", err)
				}
			})
		})
	}
}

func TestWithUploadTransferDeadlineShortCircuit(t *testing.T) {
	failure := errors.New("fixture deadline failure")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, release := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer release()
	for _, fixture := range []struct {
		name   string
		ctx    context.Context
		writer http.ResponseWriter
		want   error
	}{
		{"nil context", nil, httptest.NewRecorder(), nil},
		{"nil writer", context.Background(), nil, nil},
		{"canceled", canceled, &uploadDeadlineWriter{}, context.Canceled},
		{"expired", expired, &uploadDeadlineWriter{}, ErrUploadTransferTimeout},
		{"unsupported", context.Background(), httptest.NewRecorder(), http.ErrNotSupported},
		{"setting failed", context.Background(), &uploadDeadlineWriter{failure: failure}, failure},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			err := WithUploadTransferDeadline(fixture.ctx, fixture.writer, func(context.Context) error {
				t.Fatal("设置期限前执行了传输")
				return nil
			})
			if err == nil || (fixture.want != nil && !errors.Is(err, fixture.want)) {
				t.Fatal("设置读取期限的错误分类异常", err)
			}
			if writer, ok := fixture.writer.(*uploadDeadlineWriter); ok && fixture.ctx != nil && fixture.ctx.Err() != nil && len(writer.deadlines) != 0 {
				t.Fatal("已结束请求设置了读取期限")
			}
		})
	}
	writer := &uploadDeadlineWriter{}
	if err := WithUploadTransferDeadline(context.Background(), writer, nil); err == nil || len(writer.deadlines) != 0 {
		t.Fatal("空传输回调未短路", err)
	}
}

func TestWithUploadTransferDeadlineErrors(t *testing.T) {
	failure := errors.New("fixture transfer failure")
	readTimeout := fmt.Errorf("fixture socket timeout: %w", os.ErrDeadlineExceeded)
	for _, fixture := range []struct {
		name     string
		returned error
		expire   bool
		cancel   bool
		want     error
	}{
		{"ordinary transfer failure", failure, false, false, nil},
		{"socket timeout", readTimeout, false, false, context.DeadlineExceeded},
		{"context deadline", nil, true, false, context.DeadlineExceeded},
		{"deadline and transfer error", failure, true, false, context.DeadlineExceeded},
		{"parent cancellation", failure, false, true, context.Canceled},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, cancel := context.WithCancel(t.Context())
				defer cancel()
				err := WithUploadTransferDeadline(parent, &uploadDeadlineWriter{}, func(ctx context.Context) error {
					if fixture.expire {
						<-ctx.Done()
					}
					if fixture.cancel {
						cancel()
					}
					return fixture.returned
				})
				if err == nil || (fixture.returned != nil && !errors.Is(err, fixture.returned)) ||
					(fixture.want != nil && !errors.Is(err, fixture.want)) ||
					errors.Is(err, ErrUploadTransferTimeout) != (fixture.want == context.DeadlineExceeded) ||
					(fixture.want == nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled))) {
					t.Fatal("传输错误或超时原因丢失", err)
				}
			})
		})
	}
}

func TestWithUploadTransferDeadlinePreservesDependencyDeadline(t *testing.T) {
	dependencyError := fmt.Errorf("fixture dependency deadline: %w", context.DeadlineExceeded)
	err := WithUploadTransferDeadline(t.Context(), &uploadDeadlineWriter{}, func(context.Context) error {
		return dependencyError
	})
	if err != dependencyError || errors.Is(err, ErrUploadTransferTimeout) {
		t.Fatal("依赖超时被当成上传 HTTP 期限到达", err)
	}
}
