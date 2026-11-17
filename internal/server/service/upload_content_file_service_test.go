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

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestNewUploadContentFileService(t *testing.T) {
	space := NewUploadTempSpace()
	directory := t.TempDir()
	for _, fixture := range []struct {
		name      string
		directory string
		space     *UploadTempSpace
		valid     bool
	}{
		{"valid", directory, space, true},
		{"empty directory", "", space, false},
		{"nil space", directory, nil, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			service, err := NewUploadContentFileService(fixture.directory, fixture.space)
			if !fixture.valid {
				if err == nil || service != nil {
					t.Fatal("无效构造参数被接受")
				}
				return
			}
			if err != nil || service == nil || service.space != space || service.directory != directory || service.pending == nil || len(service.pending) != 0 || space.reservedBytes != 0 || space.activeReceivers != 0 {
				t.Fatal("构造结果错误", err)
			}
		})
	}
}

func TestUploadContentFileServiceFlow(t *testing.T) {
	useFailure := errors.New("fixture use failure")
	readFailure := errors.New("fixture read failure")
	matching := sha256.Sum256([]byte("abc"))
	wrong := [sha256.Size]byte{}
	for _, fixture := range []struct {
		name     string
		payload  string
		declared uint64
		expected *[sha256.Size]byte
		want     error
		calls    int
	}{
		{"success", "abc", 3, nil, nil, 1},
		{"empty", "", 0, nil, nil, 1},
		{"matching", "abc", 3, &matching, nil, 1},
		{"short", "ab", 3, nil, domain.ErrUploadBodySizeMismatch, 0},
		{"long", "abcd", 3, nil, domain.ErrUploadBodySizeMismatch, 0},
		{"checksum", "abc", 3, &wrong, domain.ErrUploadChecksumMismatch, 0},
		{"source failure", "abc", 3, nil, readFailure, 0},
		{"consumer failure", "abc", 3, nil, useFailure, 1},
		{"missing directory", "abc", 3, nil, os.ErrNotExist, 0},
		{"cancel after read", "abc", 3, nil, context.Canceled, 0},
		{"cancel in consumer", "abc", 3, nil, context.Canceled, 1},
		{"panic in consumer", "abc", 3, nil, nil, 1},
		{"consumer closes file", "abc", 3, nil, os.ErrClosed, 1},
		{"consumer removes file", "abc", 3, nil, os.ErrNotExist, 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			directory := t.TempDir()
			space := NewUploadTempSpace()
			service, err := NewUploadContentFileService(directory, space)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.name == "missing directory" {
				service.directory = filepath.Join(directory, "missing")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var source io.Reader = bytes.NewBufferString(fixture.payload)
			if fixture.name == "source failure" {
				source = uploadCopyReadFunc(func([]byte) (int, error) { return 0, readFailure })
			}
			tracked := &uploadCopySource{reader: source}
			if fixture.name == "cancel after read" {
				tracked.afterRead = func(int) { cancel() }
			}
			calls := 0
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				err = service.WithFile(ctx, tracked, fixture.declared, fixture.expected, func(content io.ReadSeeker, digest domain.UploadContentDigest) error {
					calls++
					payload, readError := io.ReadAll(content)
					if readError != nil || string(payload) != fixture.payload || digest.SHA256 != sha256.Sum256(payload) || space.reservedBytes != fixture.declared {
						t.Fatal("交付文件、摘要或在途预算错误", readError)
					}
					switch fixture.name {
					case "consumer failure":
						return useFailure
					case "cancel in consumer":
						cancel()
					case "panic in consumer":
						panic("fixture panic")
					case "consumer closes file", "consumer removes file":
						file := content.(*os.File)
						if err := file.Close(); err != nil {
							t.Fatal(err)
						}
						if fixture.name == "consumer removes file" {
							if err := os.Remove(file.Name()); err != nil {
								t.Fatal(err)
							}
						}
					}
					return nil
				})
			}()
			entries, readError := os.ReadDir(directory)
			if !errors.Is(err, fixture.want) || calls != fixture.calls || readError != nil || len(entries) != 0 || len(service.pending) != 0 || space.reservedBytes != 0 || space.activeReceivers != 0 || tracked.closes != 0 {
				t.Fatal("结果、清理或预算释放错误", err, readError, space.reservedBytes)
			}
			if fixture.name == "panic in consumer" {
				if recovered != "fixture panic" {
					t.Fatal("panic 载荷被改变", recovered)
				}
			} else if recovered != nil {
				t.Fatal("意外 panic", recovered)
			}
		})
	}
}

func TestUploadContentFileServiceInputsAndCapacity(t *testing.T) {
	directory := t.TempDir()
	space := NewUploadTempSpace()
	service, err := NewUploadContentFileService(directory, space)
	if err != nil {
		t.Fatal(err)
	}
	occupiedRelease, err := space.Reserve(context.Background(), uploadTempCapacityBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer occupiedRelease()
	for _, name := range []string{"nil service", "zero service", "nil context", "nil source", "nil consumer", "oversized", "max uint64", "canceled", "full capacity", "zero when full"} {
		t.Run(name, func(t *testing.T) {
			receiver := service
			var ctx context.Context = context.Background()
			source := &uploadCopySource{reader: bytes.NewReader(nil)}
			var reader io.Reader = source
			declared := uint64(1)
			calls := 0
			use := func(io.ReadSeeker, domain.UploadContentDigest) error { calls++; return nil }
			var want error
			switch name {
			case "nil service":
				receiver = nil
			case "zero service":
				receiver = &UploadContentFileService{}
			case "nil context":
				ctx = nil
			case "nil source":
				reader = nil
			case "nil consumer":
				use = nil
			case "oversized":
				declared = domain.MaxSingleUploadSizeBytes + 1
				want = domain.ErrUploadFileTooLarge
			case "max uint64":
				declared = math.MaxUint64
				want = domain.ErrUploadFileTooLarge
			case "canceled":
				canceled, cancel := context.WithCancel(context.Background())
				cancel()
				ctx, want = canceled, context.Canceled
			case "full capacity":
				want = domain.ErrUploadTempSpaceExhausted
			case "zero when full":
				declared = 0
			}
			err := receiver.WithFile(ctx, reader, declared, nil, use)
			if name == "zero when full" {
				if err != nil || calls != 1 {
					t.Fatal("零字节接收错误", err)
				}
			} else if err == nil || (want != nil && !errors.Is(err, want)) || calls != 0 || source.reads != 0 {
				t.Fatal("参数或容量拒绝顺序错误", err)
			}
			entries, readError := os.ReadDir(directory)
			if readError != nil || len(entries) != 0 || len(service.pending) != 0 || space.reservedBytes != uploadTempCapacityBytes || space.activeReceivers != 0 || source.closes != 0 {
				t.Fatal("拒绝或零字节改变原预算/文件", readError)
			}
		})
	}
}

func TestUploadContentFileServiceCleanup(t *testing.T) {
	for _, panicInConsumer := range []bool{false, true} {
		name := "error"
		if panicInConsumer {
			name = "panic"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			space := NewUploadTempSpace()
			service, err := NewUploadContentFileService(directory, space)
			if err != nil {
				t.Fatal(err)
			}
			useFailure := errors.New("fixture cleanup failure")
			var blockedPath string
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				err = service.WithFile(context.Background(), bytes.NewBufferString("abc"), 3, nil, func(content io.ReadSeeker, _ domain.UploadContentDigest) error {
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
					if panicInConsumer {
						panic("cleanup panic")
					}
					return useFailure
				})
			}()
			if (panicInConsumer && recovered != "cleanup panic") || (!panicInConsumer && (recovered != nil || !errors.Is(err, useFailure))) || len(service.pending) != 1 || space.reservedBytes != 3 || space.activeReceivers != 0 {
				t.Fatal("失败清理登记或原始原因错误", err, recovered)
			}
			// 1. 回收仍失败或请求取消时，保留记录和预占。
			if err := service.RetryCleanup(context.Background()); err == nil || len(service.pending) != 1 || space.reservedBytes != 3 || space.activeReceivers != 0 {
				t.Fatal("失败回收提前释放预算", err)
			}
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if err := service.RetryCleanup(canceled); !errors.Is(err, context.Canceled) || len(service.pending) != 1 || space.reservedBytes != 3 || space.activeReceivers != 0 {
				t.Fatal("取消回收改变记录", err)
			}
			// 2. 其他接收可以正常完成，保留旧失败尝试的预算。
			if err := service.WithFile(context.Background(), bytes.NewBufferString("new"), 3, nil, func(io.ReadSeeker, domain.UploadContentDigest) error { return nil }); err != nil || space.reservedBytes != 3 {
				t.Fatal("旧待回收记录影响其他接收", err)
			}
			// 3. 恢复删除条件，并发重试只完成一次预算释放。
			if err := os.Remove(filepath.Join(blockedPath, "keep")); err != nil {
				t.Fatal(err)
			}
			var callers sync.WaitGroup
			for range 16 {
				callers.Go(func() {
					if err := service.RetryCleanup(context.Background()); err != nil {
						t.Error(err)
					}
				})
			}
			callers.Wait()
			if len(service.pending) != 0 || space.reservedBytes != 0 || space.activeReceivers != 0 {
				t.Fatal("完成回收后仍保留记录或预算")
			}
			if err := os.WriteFile(blockedPath, []byte("replacement"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := service.RetryCleanup(context.Background()); err != nil {
				t.Fatal(err)
			}
			if content, err := os.ReadFile(blockedPath); err != nil || string(content) != "replacement" {
				t.Fatal("旧回收记录删除了后来创建的文件", err)
			}
		})
	}
}

func TestUploadContentFileServiceRetryInputs(t *testing.T) {
	var missing *UploadContentFileService
	if missing.RetryCleanup(context.Background()) == nil || (&UploadContentFileService{}).RetryCleanup(context.Background()) == nil {
		t.Fatal("空回收服务未拒绝")
	}
	service, err := NewUploadContentFileService(t.TempDir(), NewUploadTempSpace())
	if err != nil {
		t.Fatal(err)
	}
	if service.RetryCleanup(nil) == nil {
		t.Fatal("空回收上下文未拒绝")
	}
	if err := service.RetryCleanup(context.Background()); err != nil {
		t.Fatal("空队列回收失败", err)
	}
}

func TestUploadContentFileServiceConcurrent(t *testing.T) {
	directory := t.TempDir()
	space := NewUploadTempSpace()
	service, err := NewUploadContentFileService(directory, space)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0xff}, int(domain.MaxSingleUploadSizeBytes))
	var accepted atomic.Int32
	var attempted, callers sync.WaitGroup
	proceed := make(chan struct{})
	attempted.Add(32)
	for range 32 {
		callers.Go(func() {
			entered := false
			err := service.WithFile(context.Background(), bytes.NewReader(payload), uint64(len(payload)), nil, func(io.ReadSeeker, domain.UploadContentDigest) error {
				entered = true
				accepted.Add(1)
				attempted.Done()
				<-proceed
				return nil
			})
			if !entered {
				attempted.Done()
			}
			if (entered && err != nil) || (!entered && !errors.Is(err, domain.ErrUploadReceiverBusy)) {
				t.Error("并发接收结果错误", err)
			}
		})
	}
	attempted.Wait()
	if accepted.Load() != 3 || space.activeReceivers != 3 || space.reservedBytes != 3*domain.MaxSingleUploadSizeBytes {
		t.Error("并发接收突破容量或过早释放", accepted.Load(), space.reservedBytes)
	}
	close(proceed)
	callers.Wait()
	entries, readError := os.ReadDir(directory)
	if readError != nil || len(entries) != 0 || len(service.pending) != 0 || space.reservedBytes != 0 || space.activeReceivers != 0 {
		t.Fatal("并发接收结束后的文件或预算错误", readError)
	}
}
