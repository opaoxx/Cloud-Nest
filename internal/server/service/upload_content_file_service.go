package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// UploadContentFileService 管理同一应用临时目录中的接收、预算及失败回收。
type UploadContentFileService struct {
	// directory 是调用方指定的已有应用临时目录。
	directory string
	// space 是同一临时区共享的容量预算。
	space *UploadTempSpace
	// cleanupMutex 串行化失败登记和回收重试，保护 pending。
	cleanupMutex sync.Mutex
	// pending 保留清理失败的描述符与对应的预算释放方法。
	pending map[*os.File]func()
}

// NewUploadContentFileService 绑定已有应用临时目录与共享字节预算。
func NewUploadContentFileService(directory string, space *UploadTempSpace) (*UploadContentFileService, error) {
	// 1. 检查目录和预算依赖，再初始化待回收记录。
	if directory == "" || space == nil {
		return nil, fmt.Errorf("创建上传文件服务需要临时目录和容量预算")
	}
	return &UploadContentFileService{directory: directory, space: space, pending: make(map[*os.File]func())}, nil
}

// WithFile 在容量预算内接收文件，向 use 交付定位到开头的完整内容。
// 清理失败时保留预算及回收记录，panic 保持原有传播方式。
func (service *UploadContentFileService) WithFile(ctx context.Context, source io.Reader, declaredSize uint64, expected *[sha256.Size]byte, use func(io.ReadSeeker, domain.UploadContentDigest) error) error {
	return service.withFile(ctx, source, declaredSize, expected, nil, use)
}

// WithPreparedFile 在资源预占后执行 prepare，再接收文件并交给 use。
// prepare 失败或请求取消时停止接收。两个回调均在当前调用内同步执行。
func (service *UploadContentFileService) WithPreparedFile(ctx context.Context, source io.Reader, declaredSize uint64, expected *[sha256.Size]byte, prepare func(context.Context) error, use func(io.ReadSeeker, domain.UploadContentDigest) error) error {
	// 1. 要求调用方提供正文读取前的准备步骤。
	if prepare == nil {
		return fmt.Errorf("接收上传临时文件需要准备方法")
	}

	// 2. 进入共享资源管理流程，按顺序完成准备、接收及清理。
	return service.withFile(ctx, source, declaredSize, expected, prepare, use)
}

func (service *UploadContentFileService) withFile(ctx context.Context, source io.Reader, declaredSize uint64, expected *[sha256.Size]byte, prepare func(context.Context) error, use func(io.ReadSeeker, domain.UploadContentDigest) error) error {
	// 1. 校验接收参数和服务依赖。
	if service == nil || service.directory == "" || service.space == nil || service.pending == nil {
		return fmt.Errorf("接收上传临时文件需要已初始化的文件服务")
	}
	if err := validateUploadContentFileInput(ctx, service.directory, source, declaredSize, use); err != nil {
		return err
	}

	// 2. 领取共享接收名额，返回及 panic 展开时释放。
	releaseReceiver, err := service.space.acquireReceiver(ctx)
	if err != nil {
		return err
	}
	defer releaseReceiver()

	// 3. 预占本次声明大小。
	release, err := service.space.Reserve(ctx, declaredSize)
	if err != nil {
		return err
	}

	// 4. 在资源预占内完成准备和接收，文件清理成功时释放容量。
	return withUploadContentFile(ctx, service.directory, source, declaredSize, expected, prepare, use, func(file *os.File, cleaned bool) {
		if cleaned {
			release()
			return
		}

		// 5. 清理失败时登记待回收文件，保留预算。
		service.cleanupMutex.Lock()
		defer service.cleanupMutex.Unlock()
		service.pending[file] = release
	})
}

// RetryCleanup 重试当前待回收文件，成功后释放对应预算。
// 多次或并发调用按记录串行执行，取消时保留尚未完成的记录。
func (service *UploadContentFileService) RetryCleanup(ctx context.Context) error {
	// 1. 检查上下文并锁定待回收记录。
	if service == nil || service.pending == nil || ctx == nil {
		return fmt.Errorf("重试上传文件回收需要已初始化的服务和上下文")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	service.cleanupMutex.Lock()
	defer service.cleanupMutex.Unlock()

	// 2. 逐个重试关闭和删除。
	var resultError error
	for file, release := range service.pending {
		if err := ctx.Err(); err != nil {
			return errors.Join(resultError, err)
		}
		cleaned, err := cleanupUploadContentFile(file)
		if !cleaned {
			resultError = errors.Join(resultError, err)
			continue
		}

		// 3. 确认回收完成后释放预算并移除记录。
		release()
		delete(service.pending, file)
	}

	// 4. 汇总未完成的清理错误和取消原因。
	return errors.Join(resultError, ctx.Err())
}
