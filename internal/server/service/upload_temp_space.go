package service

import (
	"context"
	"fmt"
	"sync"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// uploadTempCapacityBytes 是首期单进程上传临时区的 128 MiB 容量上限。
const uploadTempCapacityBytes uint64 = 128 * 1024 * 1024

// UploadTempSpace 并发安全地保存同一临时区的在途字节预算，零值可用。
// 同一进程的临时区使用同一实例，首次使用后通过指针共享。
type UploadTempSpace struct {
	// mutex 保护字节及接收名额计数，锁内只执行计数检查与更新。
	mutex sync.Mutex
	// reservedBytes 是尚未确认清理完成的预占总字节数。
	reservedBytes uint64
	// activeReceivers 是持有接收名额、尚未结束本次清理及登记的调用数。
	activeReceivers int
}

// NewUploadTempSpace 创建容量为 128 MiB 的空预算。
func NewUploadTempSpace() *UploadTempSpace {
	return &UploadTempSpace{}
}

// Reserve 原子预占 sizeBytes，成功返回可并发重复调用的释放方法。
// 调用方确认对应临时内容清理完成后释放，清理失败时保留预算至回收完成。
func (space *UploadTempSpace) Reserve(ctx context.Context, sizeBytes uint64) (func(), error) {
	// 1. 检查预算实例、上下文和取消状态。
	if space == nil || ctx == nil {
		return nil, fmt.Errorf("预占上传临时区需要预算实例和上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 加锁后复查取消，核对剩余容量。
	space.mutex.Lock()
	defer space.mutex.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sizeBytes > uploadTempCapacityBytes-space.reservedBytes {
		return nil, domain.ErrUploadTempSpaceExhausted
	}

	// 3. 原子增加预占，返回只能生效一次的释放方法。
	space.reservedBytes += sizeBytes
	var once sync.Once
	release := func() {
		// 4. 释放时加锁，扣回本次预占字节数。
		once.Do(func() {
			space.mutex.Lock()
			defer space.mutex.Unlock()
			space.reservedBytes -= sizeBytes
		})
	}

	return release, nil
}
