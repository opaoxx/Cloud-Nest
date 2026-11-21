package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"slices"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type verificationRenewalResult struct {
	task *domain.UploadTask
	err  error
}

func TestMultipartUploadVerificationLeaseRenewalWaitsBeforeWriteback(t *testing.T) {
	content := bytes.Repeat([]byte{0x22}, int(domain.MaxSingleUploadSizeBytes+1))
	checksum := sha256.Sum256(content)
	for _, fixture := range []struct {
		name, wantWrite string
		newReader       func() io.Reader
		renewError      error
		wantError       error
	}{
		{name: "publish after renewal", wantWrite: "publish", newReader: func() io.Reader { return bytes.NewReader(content) }},
		{name: "finalize mismatch after renewal", wantWrite: "size", newReader: func() io.Reader { return bytes.NewReader(content[:2]) }, wantError: domain.ErrUploadBodySizeMismatch},
		{name: "schedule storage retry after renewal", wantWrite: "retry", newReader: func() io.Reader { return iotest.ErrReader(domain.ErrUploadStorageUnavailable) }},
		{name: "discard completed read when in-flight renewal fails", newReader: func() io.Reader { return bytes.NewReader(content) },
			renewError: domain.ErrUploadTaskBusy, wantError: domain.ErrUploadTaskBusy},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// 1. 让真实的字节与摘要校验阻塞在第一次读取。
				parent, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				task, object := uploadMultipartObjectVerificationFixture()
				task.ExpectedSHA256 = &checksum
				allowRead, allowRenewal := make(chan struct{}), make(chan struct{})
				var openedContext context.Context
				var closed bool
				var writes []string
				recordWrite := func(ctx context.Context, branch string) {
					if !closed || ctx != parent || ctx.Err() != nil {
						t.Error("写回需要已关闭的对象流和有效的原调用上下文")
					}
					writes = append(writes, branch)
				}
				verification := NewUploadVerificationService(func(ctx context.Context, selected *domain.StoredObject) (io.ReadCloser, error) {
					if selected != object || ctx == parent {
						t.Error("读回未使用原对象与独立的可取消上下文")
					}
					openedContext = ctx
					return uploadObjectVerificationStream{uploadCopySource: &uploadCopySource{
						reader: fixture.newReader(), afterRead: func(readCount int) {
							if readCount == 1 {
								select {
								case <-allowRead:
								case <-ctx.Done():
								}
							}
						},
					}, closeStream: func() error { closed = true; return nil }}, nil
				}, func(ctx context.Context, _ *domain.UploadTask, _ *domain.StoredObject, digest domain.UploadContentDigest) (*domain.UploadTask, error) {
					recordWrite(ctx, "publish")
					if digest.SizeBytes != task.FileSize || digest.SHA256 != checksum {
						t.Error("发布未收到正确的大小和摘要")
					}
					return task, nil
				}, func(ctx context.Context, _ *domain.UploadTask, _ *domain.StoredObject, _ error) error {
					recordWrite(ctx, "checksum")
					return nil
				}, func(ctx context.Context, _ *domain.UploadTask, _ *domain.StoredObject, _ error) error {
					recordWrite(ctx, "size")
					return nil
				}, func(ctx context.Context, _ *domain.UploadTask, delay int) (*domain.UploadTask, error) {
					recordWrite(ctx, "retry")
					if delay != 1 {
						t.Error("初次存储重试应等待一秒", delay)
					}
					return task, nil
				})
				startedAt := time.Now()
				var renewals []time.Duration
				done := make(chan verificationRenewalResult, 1)
				go func() {
					result, err := verification.VerifyMultipartAndPublish(parent, task, object, func(ctx context.Context, claimed *domain.UploadTask) error {
						if ctx == parent || ctx.Err() != nil || claimed != task {
							t.Error("续约未使用读回上下文和原领取快照")
						}
						renewals = append(renewals, time.Since(startedAt))
						if len(renewals) == 2 {
							select {
							case <-allowRenewal:
								return fixture.renewError
							case <-ctx.Done():
								return ctx.Err()
							}
						}
						return nil
					})
					done <- verificationRenewalResult{result, err}
				}()

				// 2. 跨过两次续约时点，让读回在第二次续约未完成时退出。
				time.Sleep(20*time.Second - time.Nanosecond)
				synctest.Wait()
				if len(renewals) != 0 || openedContext == nil {
					t.Fatal("读回未启动或提前续约", renewals)
				}
				time.Sleep(20*time.Second + time.Nanosecond)
				synctest.Wait()
				if !slices.Equal(renewals, []time.Duration{20 * time.Second, 40 * time.Second}) {
					t.Fatal("首次或后续续约时间错误", renewals)
				}
				close(allowRead)
				time.Sleep(40 * time.Second)
				synctest.Wait()
				if !closed || openedContext.Err() != nil || len(renewals) != 2 || len(writes) != 0 || len(done) != 0 {
					t.Fatal("续约在途时提前写回、取消上下文或启动额外续约", renewals, writes)
				}

				// 3. 等待在途续约结果，再核对写回分支及停止行为。
				close(allowRenewal)
				synctest.Wait()
				if len(done) != 1 {
					t.Fatal("续约结束后校验仍未返回")
				}
				result := <-done
				var wantWrites []string
				if fixture.wantWrite != "" {
					wantWrites = []string{fixture.wantWrite}
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if !errors.Is(result.err, fixture.wantError) || !slices.Equal(writes, wantWrites) || len(renewals) != 2 ||
					(fixture.wantError == nil && result.task != task) || (fixture.wantError != nil && result.task != nil) {
					t.Fatal("写回分支、返回结果或续约停止错误", writes, result.err, renewals)
				}
			})
		})
	}
}

func TestMultipartUploadVerificationLeaseRenewalCancelsRead(t *testing.T) {
	parentError := errors.New("fixture parent canceled")
	for _, fixture := range []struct {
		name         string
		err          error
		cancelParent bool
	}{
		{name: "renewal loses lease", err: domain.ErrUploadTaskBusy},
		{name: "renewal times out", err: context.DeadlineExceeded},
		{name: "caller cancels during renewal", err: parentError, cancelParent: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				task, object := uploadMultipartObjectVerificationFixture()
				var closeCause error
				verification := NewUploadVerificationService(func(ctx context.Context, _ *domain.StoredObject) (io.ReadCloser, error) {
					return uploadObjectVerificationStream{uploadCopySource: &uploadCopySource{
						reader: bytes.NewReader([]byte("fixture")), afterRead: func(int) { <-ctx.Done() },
					}, closeStream: func() error { closeCause = context.Cause(ctx); return nil }}, nil
				}, func(context.Context, *domain.UploadTask, *domain.StoredObject, domain.UploadContentDigest) (*domain.UploadTask, error) {
					t.Error("续约失败后仍发布了文件")
					return nil, nil
				}, func(context.Context, *domain.UploadTask, *domain.StoredObject, error) error {
					t.Error("续约失败后仍写入摘要失配")
					return nil
				}, func(context.Context, *domain.UploadTask, *domain.StoredObject, error) error {
					t.Error("续约失败后仍写入大小失配")
					return nil
				}, func(context.Context, *domain.UploadTask, int) (*domain.UploadTask, error) {
					t.Error("续约失败后仍安排存储重试")
					return nil, nil
				})
				renewalCalls := 0
				done := make(chan verificationRenewalResult, 1)
				go func() {
					result, err := verification.VerifyMultipartAndPublish(parent, task, object, func(ctx context.Context, _ *domain.UploadTask) error {
						renewalCalls++
						if fixture.cancelParent {
							<-ctx.Done()
							return ctx.Err()
						}
						return fixture.err
					})
					done <- verificationRenewalResult{result, err}
				}()
				time.Sleep(20 * time.Second)
				synctest.Wait()
				if fixture.cancelParent {
					cancel(parentError)
				}
				time.Sleep(time.Minute)
				synctest.Wait()
				if len(done) != 1 || renewalCalls != 1 || closeCause != fixture.err {
					t.Fatal("续约失败未中止读回并关闭对象流", closeCause, renewalCalls)
				}
				if result := <-done; result.task != nil || result.err != fixture.err {
					t.Fatal("错误返回值未保留原始取消原因", result.err)
				}
			})
		})
	}
}
