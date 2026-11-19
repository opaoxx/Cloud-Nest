package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"slices"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestWithPreparedFileOrder(t *testing.T) {
	for _, payload := range []string{"abc", ""} {
		name := "nonempty"
		if payload == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			directory := t.TempDir()
			space := NewUploadTempSpace()
			receiver, err := NewUploadContentFileService(directory, space)
			if err != nil {
				t.Fatal(err)
			}
			declaredSize := uint64(len(payload))
			digest := sha256.Sum256([]byte(payload))
			expected := digest
			var events []string
			body := bytes.NewBufferString(payload)
			source := uploadCopyReadFunc(func(buffer []byte) (int, error) {
				if len(events) == 0 {
					t.Fatal("准备之前读取了正文")
				}
				if len(events) == 1 {
					events = append(events, "read")
				}
				return body.Read(buffer)
			})

			err = receiver.WithPreparedFile(ctx, source, declaredSize, &expected, func(prepareContext context.Context) error {
				events = append(events, "prepare")
				entries, readError := os.ReadDir(directory)
				if prepareContext != ctx || readError != nil || len(entries) != 0 || space.activeReceivers != 1 || space.reservedBytes != declaredSize {
					t.Fatal("准备应在资源预占后、创建文件前执行", readError)
				}
				// 接收使用进入准备回调前保存的摘要声明。
				expected = [sha256.Size]byte{}
				return nil
			}, func(file io.ReadSeeker, actual domain.UploadContentDigest) error {
				events = append(events, "use")
				received, readError := io.ReadAll(file)
				if readError != nil || string(received) != payload || actual.SizeBytes != declaredSize || actual.SHA256 != digest || space.activeReceivers != 1 || space.reservedBytes != declaredSize {
					t.Fatal("文件、摘要或在途资源错误", readError)
				}
				return nil
			})
			if err != nil || !slices.Equal(events, []string{"prepare", "read", "use"}) {
				t.Fatal("准备、读取、使用的顺序错误", events, err)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 || space.activeReceivers != 0 || space.reservedBytes != 0 || len(receiver.pending) != 0 {
				t.Fatal("接收结束后应完成清理和资源释放", err)
			}
		})
	}
}

func TestWithPreparedFilePreparationStopsReceive(t *testing.T) {
	prepareFailure := errors.New("fixture prepare failure")
	for _, fixture := range []struct {
		name    string
		failure error
		cancel  bool
		panics  bool
	}{
		{name: "failed", failure: prepareFailure},
		{name: "canceled", cancel: true},
		{name: "failed and canceled", failure: prepareFailure, cancel: true},
		{name: "panic", panics: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			directory := t.TempDir()
			space := NewUploadTempSpace()
			receiver, err := NewUploadContentFileService(directory, space)
			if err != nil {
				t.Fatal(err)
			}
			source := &uploadCopySource{reader: bytes.NewBufferString("abc")}
			calls := 0
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				err = receiver.WithPreparedFile(ctx, source, 3, nil, func(context.Context) error {
					calls++
					if fixture.cancel {
						cancel()
					}
					if fixture.panics {
						panic(prepareFailure)
					}
					return fixture.failure
				}, func(io.ReadSeeker, domain.UploadContentDigest) error {
					t.Fatal("准备中断后调用了文件使用方")
					return nil
				})
			}()

			if fixture.panics {
				if recovered != prepareFailure {
					t.Fatal("准备回调的 panic 未原样传播", recovered)
				}
			} else if recovered != nil || err == nil ||
				(fixture.failure != nil && !errors.Is(err, fixture.failure)) ||
				(fixture.cancel && !errors.Is(err, context.Canceled)) {
				t.Fatal("准备错误或取消原因错误", err, recovered)
			}
			entries, readError := os.ReadDir(directory)
			if calls != 1 || source.reads != 0 || source.closes != 0 || readError != nil || len(entries) != 0 || space.activeReceivers != 0 || space.reservedBytes != 0 || len(receiver.pending) != 0 {
				t.Fatal("准备中断后发生正文读写或资源未释放", err, readError)
			}
		})
	}
}

func TestWithPreparedFileAdmissionBeforePreparation(t *testing.T) {
	for _, name := range []string{"missing prepare", "missing use", "canceled", "oversized", "no slot", "no capacity"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			directory := t.TempDir()
			space := NewUploadTempSpace()
			receiver, err := NewUploadContentFileService(directory, space)
			if err != nil {
				t.Fatal(err)
			}
			source := &uploadCopySource{reader: bytes.NewBufferString("abc")}
			declaredSize := uint64(3)
			prepare := func(context.Context) error {
				t.Fatal("资源未就绪时调用了准备方法")
				return nil
			}
			use := func(io.ReadSeeker, domain.UploadContentDigest) error {
				t.Fatal("接收被拒绝后调用了文件使用方")
				return nil
			}
			var want error
			switch name {
			case "missing prepare":
				prepare = nil
			case "missing use":
				use = nil
			case "canceled":
				cancel()
				want = context.Canceled
			case "oversized":
				declaredSize = domain.MaxSingleUploadSizeBytes + 1
				want = domain.ErrUploadFileTooLarge
			case "no slot":
				for range maxUploadReceivers {
					release, err := space.acquireReceiver(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer release()
				}
				want = domain.ErrUploadReceiverBusy
			case "no capacity":
				release, err := space.Reserve(ctx, uploadTempCapacityBytes)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				want = domain.ErrUploadTempSpaceExhausted
			}

			previousReceivers, previousBytes := space.activeReceivers, space.reservedBytes
			err = receiver.WithPreparedFile(ctx, source, declaredSize, nil, prepare, use)
			if err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatal("接收拒绝原因错误", err)
			}
			entries, readError := os.ReadDir(directory)
			if source.reads != 0 || readError != nil || len(entries) != 0 || space.activeReceivers != previousReceivers || space.reservedBytes != previousBytes {
				t.Fatal("拒绝后发生正文读写或已有预占改变", readError)
			}
		})
	}
}
