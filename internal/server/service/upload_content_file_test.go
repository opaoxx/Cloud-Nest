package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestClassifyUploadTempWriteError(t *testing.T) {
	// 1. 核对写盘错误分类保留原错误，并区分空间不足与其他失败。
	for _, fixture := range []struct {
		name      string
		cause     error
		exhausted bool
	}{
		{"POSIX ENOSPC", syscall.ENOSPC, true},
		{"Windows ERROR_HANDLE_DISK_FULL", syscall.Errno(39), runtime.GOOS == "windows"},
		{"Windows ERROR_DISK_FULL", syscall.Errno(112), runtime.GOOS == "windows"},
		{"permission", os.ErrPermission, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			writeError := &os.PathError{Op: "write", Path: "fixture.part", Err: fixture.cause}
			result := classifyUploadTempWriteError(writeError)
			if !errors.Is(result, writeError) || errors.Is(result, domain.ErrUploadTempSpaceExhausted) != fixture.exhausted {
				t.Fatal("暂存写入错误分类不符", result)
			}
		})
	}
	if err := classifyUploadTempWriteError(nil); err != nil {
		t.Fatal("成功写入被错误分类", err)
	}
}

func TestWithUploadContentFileKeepsSourceError(t *testing.T) {
	// 1. 即使来源返回同名错误，也保持来源失败的原有语义。
	source := uploadCopyReadFunc(func([]byte) (int, error) { return 0, syscall.ENOSPC })
	err := WithUploadContentFile(t.Context(), t.TempDir(), source, 1, nil, func(io.ReadSeeker, domain.UploadContentDigest) error {
		t.Error("来源失败仍交付了临时文件")
		return nil
	})
	if !errors.Is(err, syscall.ENOSPC) || errors.Is(err, domain.ErrUploadTempSpaceExhausted) {
		t.Fatal("来源读取错误被当成暂存盘写满", err)
	}
}

func TestWithUploadContentFileResults(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		content []byte
	}{
		{"empty", nil},
		{"abc", []byte("abc")},
		{"UTF8", []byte("学习资料 🌱")},
		{"binary", []byte{0, 0xff, 0xfe, 1}},
		{"multiple buffers", bytes.Repeat([]byte{0, 0xff, 1}, 30000)},
		{"SINGLE limit", bytes.Repeat([]byte{0xab}, int(domain.MaxSingleUploadSizeBytes))},
	} {
		for _, declaredChecksum := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/checksum %t", fixture.name, declaredChecksum), func(t *testing.T) {
				directory := t.TempDir()
				source := &uploadCopySource{reader: bytes.NewReader(fixture.content)}
				actual := sha256.Sum256(fixture.content)
				var expected *[sha256.Size]byte
				if declaredChecksum {
					expected = &actual
				}
				var received *os.File
				calls := 0
				err := WithUploadContentFile(context.Background(), directory, source, uint64(len(fixture.content)), expected, func(content io.ReadSeeker, digest domain.UploadContentDigest) error {
					// 1. 回调只在完整接收后进入，文件位置已回到开头。
					calls++
					received = content.(*os.File)
					position, seekError := content.Seek(0, io.SeekCurrent)
					if seekError != nil || position != 0 || filepath.Dir(received.Name()) != directory || !strings.HasPrefix(filepath.Base(received.Name()), "upload-") || !strings.HasSuffix(received.Name(), ".part") {
						t.Fatal("临时文件位置或归属错误", seekError)
					}
					// 2. 同一文件支持读取和重新定位，摘要与磁盘内容一致。
					payload, readError := io.ReadAll(content)
					if readError != nil || !bytes.Equal(payload, fixture.content) || digest.SizeBytes != uint64(len(payload)) || digest.SHA256 != sha256.Sum256(payload) {
						t.Fatal("已准备的文件与摘要不符", readError)
					}
					if _, err := content.Seek(0, io.SeekStart); err != nil {
						t.Fatal(err)
					}
					return nil
				})
				// 3. 回调结束后描述符关闭、文件删除，来源仍由调用方管理。
				if err != nil || calls != 1 || received == nil || source.closes != 0 || source.bytesRead != len(fixture.content) || actual != sha256.Sum256(fixture.content) {
					t.Fatal("临时文件交付结果或来源所有权错误", err)
				}
				if _, err := received.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
					t.Fatal("描述符仍打开", err)
				}
				if _, err := os.Stat(received.Name()); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("临时文件仍存在", err)
				}
			})
		}
	}
}

func TestWithUploadContentFileFailures(t *testing.T) {
	readFailure := errors.New("fixture read failure")
	useFailure := errors.New("fixture consumer failure")
	wrong := [sha256.Size]byte{}
	for _, fixture := range []struct {
		name     string
		source   io.Reader
		declared uint64
		expected *[sha256.Size]byte
		useError error
		want     error
		calls    int
	}{
		{"short", bytes.NewBufferString("ab"), 3, nil, nil, domain.ErrUploadBodySizeMismatch, 0},
		{"long", bytes.NewBufferString("abcd"), 3, nil, nil, domain.ErrUploadBodySizeMismatch, 0},
		{"nonempty for zero", bytes.NewBufferString("a"), 0, nil, nil, domain.ErrUploadBodySizeMismatch, 0},
		{"checksum", bytes.NewBufferString("abc"), 3, &wrong, nil, domain.ErrUploadChecksumMismatch, 0},
		{"empty checksum", bytes.NewReader(nil), 0, &wrong, nil, domain.ErrUploadChecksumMismatch, 0},
		{"read failure", uploadCopyReadFunc(func([]byte) (int, error) { return 0, readFailure }), 3, nil, nil, readFailure, 0},
		{"no progress", uploadCopyReadFunc(func([]byte) (int, error) { return 0, nil }), 3, nil, nil, io.ErrNoProgress, 0},
		{"consumer failure", bytes.NewBufferString("abc"), 3, nil, useFailure, useFailure, 1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			directory := t.TempDir()
			source := &uploadCopySource{reader: fixture.source}
			calls := 0
			err := WithUploadContentFile(context.Background(), directory, source, fixture.declared, fixture.expected, func(io.ReadSeeker, domain.UploadContentDigest) error {
				calls++
				return fixture.useError
			})
			entries, readError := os.ReadDir(directory)
			if !errors.Is(err, fixture.want) || calls != fixture.calls || readError != nil || len(entries) != 0 || source.closes != 0 || uint64(source.bytesRead) > fixture.declared+1 {
				t.Fatal("失败原因、回调次数或清理结果错误", err, readError)
			}
		})
	}
}

func TestWithUploadContentFileInputs(t *testing.T) {
	for _, name := range []string{"nil context", "empty directory", "nil source", "nil consumer", "oversized", "max uint64", "missing directory", "file as directory"} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			var ctx context.Context = context.Background()
			source := &uploadCopySource{reader: bytes.NewBufferString("abc")}
			var reader io.Reader = source
			declared := uint64(3)
			calls := 0
			use := func(io.ReadSeeker, domain.UploadContentDigest) error { calls++; return nil }
			var want error
			switch name {
			case "nil context":
				ctx = nil
			case "empty directory":
				directory = ""
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
			case "missing directory":
				directory = filepath.Join(directory, "missing")
				want = os.ErrNotExist
			case "file as directory":
				directory = filepath.Join(directory, "ordinary-file")
				if err := os.WriteFile(directory, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := WithUploadContentFile(ctx, directory, reader, declared, nil, use)
			if err == nil || (want != nil && !errors.Is(err, want)) || calls != 0 || source.reads != 0 || source.closes != 0 {
				t.Fatal("无效输入未在读取前结束", err)
			}
			if name == "file as directory" {
				content, readError := os.ReadFile(directory)
				if readError != nil || string(content) != "keep" {
					t.Fatal("已有文件被修改", readError)
				}
			}
		})
	}
}

func TestWithUploadContentFileCancellation(t *testing.T) {
	useFailure := errors.New("consumer and cancellation")
	for _, name := range []string{"before start", "deadline", "after read", "after EOF probe", "in consumer", "consumer error and cancellation"} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := error(context.Canceled)
			if name == "before start" {
				cancel()
			}
			if name == "deadline" {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer deadlineCancel()
				want = context.DeadlineExceeded
			}
			source := &uploadCopySource{reader: bytes.NewBufferString("abc")}
			source.afterRead = func(reads int) {
				if (name == "after read" && reads == 1) || (name == "after EOF probe" && reads == 2) {
					cancel()
				}
			}
			calls := 0
			err := WithUploadContentFile(ctx, directory, source, 3, nil, func(io.ReadSeeker, domain.UploadContentDigest) error {
				calls++
				cancel()
				if name == "consumer error and cancellation" {
					return useFailure
				}
				return nil
			})
			wantCalls := 0
			if name == "in consumer" || name == "consumer error and cancellation" {
				wantCalls = 1
			}
			entries, readError := os.ReadDir(directory)
			if !errors.Is(err, want) || calls != wantCalls || readError != nil || len(entries) != 0 || source.closes != 0 {
				t.Fatal("取消未正确传播或文件未清理", err, readError)
			}
			if name == "consumer error and cancellation" && !errors.Is(err, useFailure) {
				t.Fatal("同时发生的回调错误丢失", err)
			}
			if (name == "before start" || name == "deadline") && source.reads != 0 {
				t.Fatal("提前取消仍读取来源")
			}
		})
	}
}

func TestWithUploadContentFileCleanup(t *testing.T) {
	t.Run("panic preserves payload and neighboring file", func(t *testing.T) {
		directory := t.TempDir()
		neighbor := filepath.Join(directory, "keep.txt")
		if err := os.WriteFile(neighbor, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		var received *os.File
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			_ = WithUploadContentFile(context.Background(), directory, bytes.NewBufferString("abc"), 3, nil, func(content io.ReadSeeker, _ domain.UploadContentDigest) error {
				received = content.(*os.File)
				panic("fixture panic")
			})
		}()
		entries, err := os.ReadDir(directory)
		if recovered != "fixture panic" || received == nil || err != nil || len(entries) != 1 || entries[0].Name() != "keep.txt" {
			t.Fatal("panic 载荷、清理或相邻文件保护错误", recovered, err)
		}
		if _, err := received.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
			t.Fatal("panic 后描述符仍打开", err)
		}
	})
	t.Run("close failure still removes file", func(t *testing.T) {
		directory := t.TempDir()
		useFailure := errors.New("consumer failure")
		err := WithUploadContentFile(context.Background(), directory, bytes.NewBufferString("abc"), 3, nil, func(content io.ReadSeeker, _ domain.UploadContentDigest) error {
			// 故意提前关闭，验证后续清理错误与业务错误均能识别。
			if err := content.(*os.File).Close(); err != nil {
				t.Fatal(err)
			}
			return useFailure
		})
		entries, readError := os.ReadDir(directory)
		if !errors.Is(err, useFailure) || !errors.Is(err, os.ErrClosed) || readError != nil || len(entries) != 0 {
			t.Fatal("关闭失败后未继续删除或丢失原因", err, readError)
		}
	})
	t.Run("capture expected before source reads", func(t *testing.T) {
		expected := sha256.Sum256([]byte("abc"))
		source := &uploadCopySource{reader: bytes.NewBufferString("abc"), afterRead: func(int) { expected = [sha256.Size]byte{} }}
		calls := 0
		err := WithUploadContentFile(context.Background(), t.TempDir(), source, 3, &expected, func(io.ReadSeeker, domain.UploadContentDigest) error { calls++; return nil })
		if err != nil || calls != 1 {
			t.Fatal("读取期间摘要来源变化影响已接收参数", err)
		}
	})
	t.Run("remove failure preserves all causes", func(t *testing.T) {
		directory := t.TempDir()
		useFailure := errors.New("consumer failure with blocked cleanup")
		var blockedPath string
		err := WithUploadContentFile(context.Background(), directory, bytes.NewBufferString("abc"), 3, nil, func(content io.ReadSeeker, _ domain.UploadContentDigest) error {
			// 用含子文件的目录替换本次路径，受测方法会报告删除失败。
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
		if !errors.Is(err, useFailure) || !errors.Is(err, os.ErrClosed) || !strings.Contains(err.Error(), "删除上传临时文件失败") {
			t.Fatal("删除失败或其他错误原因丢失", err)
		}
		if content, err := os.ReadFile(filepath.Join(blockedPath, "keep")); err != nil || string(content) != "keep" {
			t.Fatal("清理越过了本次文件边界", err)
		}
		if err := os.Remove(filepath.Join(blockedPath, "keep")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(blockedPath); err != nil {
			t.Fatal(err)
		}
	})
}

func TestWithUploadContentFileConcurrent(t *testing.T) {
	directory := t.TempDir()
	var names sync.Map
	var callers sync.WaitGroup
	for index := range 16 {
		callers.Go(func() {
			payload := []byte(fmt.Sprintf("caller %d", index))
			err := WithUploadContentFile(context.Background(), directory, bytes.NewReader(payload), uint64(len(payload)), nil, func(content io.ReadSeeker, digest domain.UploadContentDigest) error {
				name := content.(*os.File).Name()
				if _, existed := names.LoadOrStore(name, true); existed {
					t.Error("并发接收复用了文件名")
				}
				actual, err := io.ReadAll(content)
				if err != nil || !bytes.Equal(actual, payload) || digest.SHA256 != sha256.Sum256(payload) {
					t.Error("并发内容混用", err)
				}
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		})
	}
	callers.Wait()
	entries, err := os.ReadDir(directory)
	count := 0
	names.Range(func(any, any) bool { count++; return true })
	if err != nil || len(entries) != 0 || count != 16 {
		t.Fatal("并发临时文件清理或数量错误", err, count)
	}
}
