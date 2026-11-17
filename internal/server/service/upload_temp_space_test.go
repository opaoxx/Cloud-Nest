package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadTempSpacePolicy(t *testing.T) {
	if uploadTempCapacityBytes != 128*1024*1024 || domain.ErrUploadTempSpaceExhausted.Error() != "上传临时空间不足，请稍后重试" {
		t.Fatal("默认容量或固定提示不符")
	}
	for _, fixture := range []struct {
		name           string
		occupied, size uint64
		want           error
	}{
		{"empty reservation", 0, 0, nil},
		{"one byte", 0, 1, nil},
		{"full capacity", 0, uploadTempCapacityBytes, nil},
		{"exact remainder", uploadTempCapacityBytes - 4, 4, nil},
		{"over remainder", uploadTempCapacityBytes - 4, 5, domain.ErrUploadTempSpaceExhausted},
		{"full rejects byte", uploadTempCapacityBytes, 1, domain.ErrUploadTempSpaceExhausted},
		{"zero when full", uploadTempCapacityBytes, 0, nil},
		{"over total", 0, uploadTempCapacityBytes + 1, domain.ErrUploadTempSpaceExhausted},
		{"max uint64", 1, math.MaxUint64, domain.ErrUploadTempSpaceExhausted},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			space := NewUploadTempSpace()
			occupiedRelease, err := space.Reserve(context.Background(), fixture.occupied)
			if err != nil {
				t.Fatal(err)
			}
			defer occupiedRelease()
			release, err := space.Reserve(context.Background(), fixture.size)
			if !errors.Is(err, fixture.want) {
				t.Fatal("容量分类错误", err)
			}
			if fixture.want != nil {
				if release != nil || space.reservedBytes != fixture.occupied {
					t.Fatal("拒绝仍交付释放方法或修改预算")
				}
				return
			}
			if release == nil || space.reservedBytes != fixture.occupied+fixture.size {
				t.Fatal("成功预占计数错误")
			}
			release()
			release()
			if space.reservedBytes != fixture.occupied {
				t.Fatal("重复释放改变其他预算")
			}
		})
	}
	var zeroValue UploadTempSpace
	release, err := zeroValue.Reserve(context.Background(), 1)
	if err != nil || release == nil {
		t.Fatal("零值预算不可用", err)
	}
	release()
}

// uploadTempObservedContext 让测试在首次取消检查完成后再取消等待中的请求。
type uploadTempObservedContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (ctx *uploadTempObservedContext) Err() error {
	err := ctx.Context.Err()
	ctx.once.Do(func() { close(ctx.checked) })
	return err
}

func TestUploadTempSpaceInputsAndCancellation(t *testing.T) {
	space := NewUploadTempSpace()
	var missing *UploadTempSpace
	if release, err := missing.Reserve(context.Background(), 1); err == nil || release != nil {
		t.Fatal("空预算未拒绝")
	}
	if release, err := space.Reserve(nil, 1); err == nil || release != nil {
		t.Fatal("空上下文未拒绝")
	}
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		want := error(context.Canceled)
		if deadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			want = context.DeadlineExceeded
		} else {
			cancel()
		}
		release, err := space.Reserve(ctx, 1)
		cancel()
		if !errors.Is(err, want) || release != nil || space.reservedBytes != 0 {
			t.Fatal("已结束上下文仍占用预算", err)
		}
	}
	// 1. 固定首次检查成功，等待计数锁期间取消。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := &uploadTempObservedContext{Context: ctx, checked: make(chan struct{})}
	completed := make(chan error, 1)
	space.mutex.Lock()
	go func() {
		release, err := space.Reserve(observed, 1)
		if release != nil {
			release()
			completed <- errors.New("取消后仍领取预算")
			return
		}
		completed <- err
	}()
	<-observed.checked
	cancel()
	space.mutex.Unlock()
	select {
	case err := <-completed:
		if !errors.Is(err, context.Canceled) || space.reservedBytes != 0 {
			t.Fatal("获得锁后未复查取消", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消检查未结束")
	}
	// 2. 已领取预算的释放独立于原请求的取消状态。
	ctx, cancel = context.WithCancel(context.Background())
	release, err := space.Reserve(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	release()
	if space.reservedBytes != 0 {
		t.Fatal("取消后无法释放")
	}
}

func TestUploadTempSpaceRelease(t *testing.T) {
	space := NewUploadTempSpace()
	first, err := space.Reserve(context.Background(), uploadTempCapacityBytes/2)
	if err != nil {
		t.Fatal(err)
	}
	second, err := space.Reserve(context.Background(), uploadTempCapacityBytes/2)
	if err != nil {
		t.Fatal(err)
	}
	var callers sync.WaitGroup
	for range 32 {
		callers.Go(func() { first(); first() })
	}
	callers.Wait()
	if space.reservedBytes != uploadTempCapacityBytes/2 {
		t.Fatal("并发重复释放改变其他预占")
	}
	replacement, err := space.Reserve(context.Background(), uploadTempCapacityBytes/2)
	if err != nil {
		t.Fatal("已释放容量无法复用", err)
	}
	first()
	if space.reservedBytes != uploadTempCapacityBytes {
		t.Fatal("旧释放方法扣减了新预占")
	}
	second()
	replacement()
	if space.reservedBytes != 0 {
		t.Fatal("最终预算未归零")
	}
}

func TestUploadTempSpaceConcurrent(t *testing.T) {
	space := NewUploadTempSpace()
	var accepted atomic.Int32
	var attempted, callers sync.WaitGroup
	proceed := make(chan struct{})
	attempted.Add(32)
	for range 32 {
		callers.Go(func() {
			release, err := space.Reserve(context.Background(), domain.MaxSingleUploadSizeBytes)
			if err == nil && release != nil {
				accepted.Add(1)
			} else if !errors.Is(err, domain.ErrUploadTempSpaceExhausted) || release != nil {
				t.Error("并发拒绝结果错误", err)
			}
			attempted.Done()
			<-proceed
			if release != nil {
				release()
				release()
			}
		})
	}
	attempted.Wait()
	if accepted.Load() != 8 || space.reservedBytes != uploadTempCapacityBytes {
		t.Error("并发预占超出容量或误拒绝", accepted.Load(), space.reservedBytes)
	}
	close(proceed)
	callers.Wait()
	if space.reservedBytes != 0 {
		t.Fatal("并发释放后未归零")
	}
}

func TestUploadTempSpaceFileFlow(t *testing.T) {
	useFailure := errors.New("fixture use failure")
	wrong := [sha256.Size]byte{}
	for _, fixture := range []struct {
		name     string
		payload  string
		expected *[sha256.Size]byte
		useError error
		want     error
	}{
		{"success", "abc", nil, nil, nil},
		{"short", "ab", nil, nil, domain.ErrUploadBodySizeMismatch},
		{"checksum", "abc", &wrong, nil, domain.ErrUploadChecksumMismatch},
		{"consumer error", "abc", nil, useFailure, useFailure},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			space := NewUploadTempSpace()
			directory := t.TempDir()
			release, err := space.Reserve(context.Background(), 3)
			if err != nil {
				t.Fatal(err)
			}
			err = WithUploadContentFile(context.Background(), directory, bytes.NewBufferString(fixture.payload), 3, fixture.expected, func(content io.ReadSeeker, _ domain.UploadContentDigest) error {
				if space.reservedBytes != 3 {
					t.Error("使用文件期间预算已经释放")
				}
				return fixture.useError
			})
			entries, readError := os.ReadDir(directory)
			if !errors.Is(err, fixture.want) || readError != nil || len(entries) != 0 || space.reservedBytes != 3 {
				t.Fatal("清理确认前的预算或文件状态错误", err, readError)
			}
			release()
			if space.reservedBytes != 0 {
				t.Fatal("清理后预算未释放")
			}
		})
	}
	t.Run("retain until failed cleanup is recovered", func(t *testing.T) {
		space := NewUploadTempSpace()
		directory := t.TempDir()
		release, err := space.Reserve(context.Background(), 3)
		if err != nil {
			t.Fatal(err)
		}
		var blockedPath string
		err = WithUploadContentFile(context.Background(), directory, bytes.NewBufferString("abc"), 3, nil, func(content io.ReadSeeker, _ domain.UploadContentDigest) error {
			file := content.(*os.File)
			blockedPath = file.Name()
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(blockedPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(blockedPath, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(blockedPath, "keep"), []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			return useFailure
		})
		if !errors.Is(err, useFailure) || space.reservedBytes != 3 {
			t.Fatal("清理失败丢失原因或提前释放预算", err)
		}
		if rejected, err := space.Reserve(context.Background(), uploadTempCapacityBytes-2); !errors.Is(err, domain.ErrUploadTempSpaceExhausted) || rejected != nil {
			t.Fatal("遗留内容的预算被复用", err)
		}
		if err := os.Remove(filepath.Join(blockedPath, "keep")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(blockedPath); err != nil {
			t.Fatal(err)
		}
		release()
		if space.reservedBytes != 0 {
			t.Fatal("回收确认后未释放预算")
		}
	})
}
