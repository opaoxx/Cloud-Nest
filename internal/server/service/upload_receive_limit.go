package service

import (
	"context"
	"fmt"
	"sync"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// maxUploadReceivers 是同一临时区最多三个在途接收调用。
const maxUploadReceivers = 3

// acquireReceiver 原子领取接收名额，满额时立即返回领域错误。
// 与字节预算共享同一实例，释放方法在调用结束后执行。
func (space *UploadTempSpace) acquireReceiver(ctx context.Context) (func(), error) {
	// 1. 检查共享实例、上下文和取消状态。
	if space == nil || ctx == nil {
		return nil, fmt.Errorf("领取上传接收名额需要共享实例和上下文")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 2. 加锁后核对三个接收名额。
	space.mutex.Lock()
	defer space.mutex.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if space.activeReceivers >= maxUploadReceivers {
		return nil, domain.ErrUploadReceiverBusy
	}

	// 3. 增加在途计数，返回幂等释放方法。
	space.activeReceivers++
	var once sync.Once
	release := func() {
		// 4. 接收调用结束时释放本次名额。
		once.Do(func() {
			space.mutex.Lock()
			defer space.mutex.Unlock()
			space.activeReceivers--
		})
	}

	return release, nil
}
