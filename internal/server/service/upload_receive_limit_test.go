package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadReceiverLimitPolicy(t *testing.T) {
	if maxUploadReceivers != 3 || domain.ErrUploadReceiverBusy.Error() != "上传接收繁忙，请稍后重试" {
		t.Fatal("接收上限或固定提示错误")
	}
	var space UploadTempSpace
	var releases []func()
	for range 3 {
		release, err := space.acquireReceiver(context.Background())
		if err != nil || release == nil {
			t.Fatal("领取可用名额失败", err)
		}
		releases = append(releases, release)
	}
	if release, err := space.acquireReceiver(context.Background()); !errors.Is(err, domain.ErrUploadReceiverBusy) || release != nil || space.activeReceivers != 3 || space.reservedBytes != 0 {
		t.Fatal("第四个接收未被拒绝或影响计数", err)
	}
	// 1. 多个调用方重复释放同一个名额，其他名额继续保留。
	var callers sync.WaitGroup
	for range 16 {
		callers.Go(func() { releases[0](); releases[0]() })
	}
	callers.Wait()
	if space.activeReceivers != 2 {
		t.Fatal("重复释放改变其他名额")
	}
	replacement, err := space.acquireReceiver(context.Background())
	if err != nil {
		t.Fatal("释放后的名额无法复用", err)
	}
	// 2. 旧释放方法保留后来领取的名额，最后全部归还。
	releases[0]()
	if space.activeReceivers != 3 {
		t.Fatal("旧释放方法扣减新名额")
	}
	releases[1]()
	releases[2]()
	replacement()
	if space.activeReceivers != 0 {
		t.Fatal("名额未归零")
	}
}

func TestUploadReceiverLimitCancellation(t *testing.T) {
	space := NewUploadTempSpace()
	var missing *UploadTempSpace
	if release, err := missing.acquireReceiver(context.Background()); err == nil || release != nil {
		t.Fatal("空实例未拒绝")
	}
	if release, err := space.acquireReceiver(nil); err == nil || release != nil {
		t.Fatal("空上下文未拒绝")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if release, err := space.acquireReceiver(ctx); !errors.Is(err, context.Canceled) || release != nil {
		t.Fatal("已取消请求仍领取名额", err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if release, err := space.acquireReceiver(ctx); !errors.Is(err, context.DeadlineExceeded) || release != nil {
		t.Fatal("已到期请求仍领取名额", err)
	}
	// 1. 首次检查结束后取消，核对获得计数锁后的复查。
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	observed := &uploadTempObservedContext{Context: ctx, checked: make(chan struct{})}
	completed := make(chan error, 1)
	space.mutex.Lock()
	go func() {
		release, err := space.acquireReceiver(observed)
		if release != nil {
			release()
			completed <- errors.New("等待期间取消仍取得名额")
			return
		}
		completed <- err
	}()
	select {
	case <-observed.checked:
		cancel()
		space.mutex.Unlock()
	case <-time.After(5 * time.Second):
		space.mutex.Unlock()
		t.Fatal("未进入首次上下文检查")
	}
	select {
	case err := <-completed:
		if !errors.Is(err, context.Canceled) || space.activeReceivers != 0 {
			t.Fatal("取消后仍占用名额", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待期间取消未结束")
	}
	// 2. 释放已领取的名额独立于请求取消。
	ctx, cancel = context.WithCancel(context.Background())
	release, err := space.acquireReceiver(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	release()
	if space.activeReceivers != 0 {
		t.Fatal("取消后无法释放名额")
	}
}

func TestUploadReceiverLimitSharedServices(t *testing.T) {
	directory := t.TempDir()
	space := NewUploadTempSpace()
	first, err := NewUploadContentFileService(directory, space)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewUploadContentFileService(directory, space)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 3)
	proceed := make(chan struct{})
	var callers sync.WaitGroup
	defer func() {
		close(proceed)
		callers.Wait()
	}()
	for range 3 {
		callers.Go(func() {
			// 在零字节文件的 EOF 检测中暂停，名额覆盖实际读取阶段。
			source := uploadCopyReadFunc(func([]byte) (int, error) {
				entered <- struct{}{}
				<-proceed
				return 0, io.EOF
			})
			if err := first.WithFile(context.Background(), source, 0, nil, func(io.ReadSeeker, domain.UploadContentDigest) error { return nil }); err != nil {
				t.Error(err)
			}
		})
	}
	for range 3 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("接收未进入实际读取")
		}
	}
	source := &uploadCopySource{reader: bytes.NewReader(nil)}
	calls := 0
	err = second.WithFile(context.Background(), source, 0, nil, func(io.ReadSeeker, domain.UploadContentDigest) error { calls++; return nil })
	entries, readError := os.ReadDir(directory)
	if !errors.Is(err, domain.ErrUploadReceiverBusy) || calls != 0 || source.reads != 0 || source.closes != 0 || space.reservedBytes != 0 || space.activeReceivers != 3 || readError != nil || len(entries) != 3 {
		t.Fatal("跨服务实例或零字节绕过接收上限", err, readError)
	}
	if err := second.WithFile(context.Background(), source, domain.MaxSingleUploadSizeBytes+1, nil, func(io.ReadSeeker, domain.UploadContentDigest) error { return nil }); !errors.Is(err, domain.ErrUploadFileTooLarge) {
		t.Fatal("参数校验未先于接收名额", err)
	}
}
