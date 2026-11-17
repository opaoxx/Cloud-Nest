package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type uploadCopyReadFunc func([]byte) (int, error)

func (read uploadCopyReadFunc) Read(buffer []byte) (int, error) { return read(buffer) }

// uploadCopySource 记录实际取出量，并暴露复制快捷接口以验证上下文包装。
type uploadCopySource struct {
	reader    io.Reader
	bytesRead int
	reads     int
	maxBuffer int
	closes    int
	shortcut  bool
	afterRead func(int)
}

func (source *uploadCopySource) Read(buffer []byte) (int, error) {
	source.reads++
	if len(buffer) > source.maxBuffer {
		source.maxBuffer = len(buffer)
	}
	count, err := source.reader.Read(buffer)
	source.bytesRead += count
	if source.afterRead != nil {
		source.afterRead(source.reads)
	}
	return count, err
}

func (source *uploadCopySource) Close() error { source.closes++; return nil }
func (source *uploadCopySource) WriteTo(io.Writer) (int64, error) {
	source.shortcut = true
	return 0, errors.New("unexpected WriterTo shortcut")
}

type uploadCopyTarget struct {
	content    bytes.Buffer
	writes     int
	closes     int
	shortcut   bool
	write      func([]byte) (int, error)
	afterWrite func()
}

func (target *uploadCopyTarget) Write(content []byte) (int, error) {
	target.writes++
	var count int
	var err error
	if target.write != nil {
		count, err = target.write(content)
	} else {
		count, err = target.content.Write(content)
	}
	if target.afterWrite != nil {
		target.afterWrite()
	}
	return count, err
}

func (target *uploadCopyTarget) Close() error { target.closes++; return nil }
func (target *uploadCopyTarget) ReadFrom(io.Reader) (int64, error) {
	target.shortcut = true
	return 0, errors.New("unexpected ReaderFrom shortcut")
}

func TestCopyUploadContentResults(t *testing.T) {
	for _, fixture := range []struct {
		name, content, digest string
	}{
		{"empty", "", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", "abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
		{"hello", "hello", "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"},
		{"UTF8", "学习资料 🌱", ""},
		{"binary", "\x00\xff\xfe\x01\x80\x00", ""},
		{"multiple buffers", string(bytes.Repeat([]byte{0xff, 0, 'a'}, 30000)), ""},
		{"SINGLE limit", string(bytes.Repeat([]byte{0xab}, int(domain.MaxSingleUploadSizeBytes))), ""},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 同一流经普通、逐字节和随数据返回 EOF 的读取器。
			modes := []string{"ordinary", "data and EOF"}
			if len(fixture.content) < 1024 {
				modes = append(modes, "one byte")
			}
			for _, mode := range modes {
				t.Run(mode, func(t *testing.T) {
					var reader io.Reader = bytes.NewBufferString(fixture.content)
					if mode == "one byte" {
						reader = iotest.OneByteReader(reader)
					}
					if mode == "data and EOF" {
						reader = iotest.DataErrReader(reader)
					}
					source := &uploadCopySource{reader: reader}
					target := &uploadCopyTarget{}
					result, err := CopyUploadContent(context.Background(), target, source, uint64(len(fixture.content)))
					if err != nil || result == nil || result.SizeBytes != uint64(len(fixture.content)) || target.content.String() != fixture.content || result.SHA256 != sha256.Sum256([]byte(fixture.content)) {
						t.Fatal("完整内容复制与摘要错误", err)
					}
					if fixture.digest != "" && hex.EncodeToString(result.SHA256[:]) != fixture.digest {
						t.Fatal("固定 SHA256 向量不符")
					}
					// 2. 数据按缓冲传递，底层快捷复制和资源关闭交给明确的边界。
					if source.bytesRead != len(fixture.content) || source.maxBuffer > 32*1024 || source.shortcut || target.shortcut || source.closes != 0 || target.closes != 0 {
						t.Fatal("读取预算、上下文边界或流所有权错误")
					}
				})
			}
		})
	}
}

func TestCopyUploadContentSizeMismatch(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		content  string
		declared uint64
	}{
		{"empty for nonempty", "", 4},
		{"short", "abc", 4},
		{"extra byte", "abcde", 4},
		{"many extra bytes", string(bytes.Repeat([]byte{'a'}, 65536)), 4},
		{"nonempty for zero", "a", 0},
		{"limit extra byte", string(bytes.Repeat([]byte{'a'}, int(domain.MaxSingleUploadSizeBytes)+1)), domain.MaxSingleUploadSizeBytes},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			source := &uploadCopySource{reader: bytes.NewBufferString(fixture.content)}
			target := &uploadCopyTarget{}
			result, err := CopyUploadContent(context.Background(), target, source, fixture.declared)
			if result != nil || !errors.Is(err, domain.ErrUploadBodySizeMismatch) || err.Error() != "文件实际大小与声明不一致" {
				t.Fatal("实际大小不符未返回固定错误及空结果", err)
			}
			if uint64(source.bytesRead) > fixture.declared+1 || uint64(target.content.Len()) > fixture.declared || source.closes != 0 || target.closes != 0 {
				t.Fatal("超长检测超出读取/写入预算或关闭了调用方资源")
			}
		})
	}
}

func TestCopyUploadContentIOFailures(t *testing.T) {
	fault := errors.New("private upload IO failure")
	for _, mode := range []string{"initial read", "partial read", "probe read", "read with data failure", "write", "partial write", "short write", "empty reads", "empty probe", "empty after progress", "temporary empty reads"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 正常 EOF 才进入大小判断，传输及写入错误保留原始原因。
			var reader io.Reader = bytes.NewBufferString("abc")
			declared, wantError := uint64(3), fault
			target := &uploadCopyTarget{}
			switch mode {
			case "initial read":
				reader = iotest.ErrReader(fault)
			case "partial read":
				reader, declared = io.MultiReader(bytes.NewBufferString("ab"), iotest.ErrReader(fault)), 3
			case "probe read":
				reader = io.MultiReader(bytes.NewBufferString("abc"), iotest.ErrReader(fault))
			case "read with data failure":
				reader = uploadCopyReadFunc(func(buffer []byte) (int, error) { return copy(buffer, "abc"), fault })
			case "write":
				target.write = func([]byte) (int, error) { return 0, fault }
			case "partial write":
				target.write = func(content []byte) (int, error) {
					count, _ := target.content.Write(content[:1])
					return count, fault
				}
			case "short write":
				target.write = func(content []byte) (int, error) { return target.content.Write(content[:1]) }
				wantError = io.ErrShortWrite
			case "empty reads", "empty probe", "empty after progress":
				reader = uploadCopyReadFunc(func([]byte) (int, error) { return 0, nil })
				wantError = io.ErrNoProgress
				if mode == "empty probe" {
					declared = 0
				}
				if mode == "empty after progress" {
					reader = io.MultiReader(bytes.NewBufferString("a"), reader)
				}
			case "temporary empty reads":
				content := bytes.NewBufferString("abc")
				emptyReads := 0
				reader = uploadCopyReadFunc(func(buffer []byte) (int, error) {
					if emptyReads < 99 {
						emptyReads++
						return 0, nil
					}
					emptyReads = 0
					return content.Read(buffer[:1])
				})
				wantError = nil
			}
			source := &uploadCopySource{reader: reader}
			result, err := CopyUploadContent(context.Background(), target, source, declared)
			if !errors.Is(err, wantError) || (wantError != nil && result != nil) || (wantError == nil && (result == nil || target.content.String() != "abc")) {
				t.Fatal("I/O 原因或结果错误", err)
			}
			if uint64(source.bytesRead) > declared+1 || uint64(target.content.Len()) > declared || source.closes != 0 || target.closes != 0 {
				t.Fatal("I/O 失败后的预算或流所有权错误")
			}
		})
	}
}

func TestCopyUploadContentInputsAndCancellation(t *testing.T) {
	for _, mode := range []string{"nil context", "nil source", "nil destination", "oversize declaration", "maximum declaration", "cancelled", "expired", "after read", "after write", "probe extra cancelled", "probe EOF cancelled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := &uploadCopySource{reader: bytes.NewBufferString("abc")}
			target := &uploadCopyTarget{}
			var reader io.Reader = source
			var writer io.Writer = target
			declared := uint64(3)
			var wantError error
			switch mode {
			case "nil context":
				ctx = nil
			case "nil source":
				reader = nil
			case "nil destination":
				writer = nil
			case "oversize declaration":
				declared, wantError = domain.MaxSingleUploadSizeBytes+1, domain.ErrUploadFileTooLarge
			case "maximum declaration":
				declared, wantError = math.MaxUint64, domain.ErrUploadFileTooLarge
			case "cancelled":
				cancel()
				wantError = context.Canceled
			case "expired":
				expired, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
				ctx, wantError = expired, context.DeadlineExceeded
			case "after read":
				source.afterRead = func(int) { cancel() }
				wantError = context.Canceled
			case "after write":
				target.afterWrite = cancel
				wantError = context.Canceled
			case "probe extra cancelled", "probe EOF cancelled":
				if mode == "probe extra cancelled" {
					source.reader = bytes.NewBufferString("abcd")
				}
				source.afterRead = func(reads int) {
					if reads == 2 {
						cancel()
					}
				}
				wantError = context.Canceled
			}
			result, err := CopyUploadContent(ctx, writer, reader, declared)
			if result != nil || err == nil || (wantError != nil && !errors.Is(err, wantError)) {
				t.Fatal("无效或取消复制未返回空结果和原始原因", err)
			}
			if source.closes != 0 || target.closes != 0 || target.content.Len() > 3 {
				t.Fatal("取消后的流所有权或写入量错误")
			}
			if mode == "after read" && target.writes != 0 {
				t.Fatal("已观察到读取取消后仍写入目标")
			}
			if wantError == domain.ErrUploadFileTooLarge && (source.reads != 0 || target.writes != 0) {
				t.Fatal("声明超限仍访问 I/O")
			}
		})
	}
}

func TestCopyUploadContentFiles(t *testing.T) {
	for _, mode := range []string{"complete", "short", "long", "closed destination"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 调用方创建并管理暂存文件，复制后核对磁盘实际内容。
			path := filepath.Join(t.TempDir(), "upload.part")
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() })
			payload := []byte{0, 0xff, 0xfe, 1}
			declared := uint64(4)
			var wantError error
			switch mode {
			case "short":
				payload, wantError = payload[:3], domain.ErrUploadBodySizeMismatch
			case "long":
				payload, wantError = append(payload, 2), domain.ErrUploadBodySizeMismatch
			case "closed destination":
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				wantError = os.ErrClosed
			}
			result, err := CopyUploadContent(context.Background(), file, bytes.NewReader(payload), declared)
			if !errors.Is(err, wantError) || (wantError != nil && result != nil) {
				t.Fatal("真实文件复制结果错误", err)
			}
			if mode == "closed destination" {
				return
			}
			if err := file.Sync(); err != nil {
				t.Fatal(err)
			}
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			content, err := io.ReadAll(file)
			if err != nil || uint64(len(content)) > declared {
				t.Fatal("暂存文件内容或大小错误", err)
			}
			if mode == "complete" && (result == nil || result.SHA256 != sha256.Sum256(content) || !bytes.Equal(content, payload)) {
				t.Fatal("磁盘内容与交付摘要不一致")
			}
		})
	}
}

func TestCopyUploadContentConcurrent(t *testing.T) {
	payload := []byte("concurrent upload\x00\xff")
	var workers sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 10; attempt++ {
				var target bytes.Buffer
				result, err := CopyUploadContent(context.Background(), &target, bytes.NewReader(payload), uint64(len(payload)))
				if err != nil || result == nil || result.SHA256 != sha256.Sum256(payload) || !bytes.Equal(target.Bytes(), payload) {
					t.Error("并发复制结果相互干扰", err)
				}
			}
		}()
	}
	workers.Wait()
}
