package service

import (
	"fmt"
	"net/netip"
	"regexp"
	"sync"
	"time"
)

const loginFailureLimit = 10
const loginFailureWindow = time.Minute
const maxLoginLimitRecords = 4096

// loginLimitUsernamePattern 同时限定用户名字符和键长度，不接受未经规范化的变体。
var loginLimitUsernamePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// loginLimitKey 分开保存 IP 与用户名维度，避免将二者只合并成一个配对键。
type loginLimitKey struct {
	// scope 为 ip 或 username。
	scope string
	// value 为规范 IP 或最多 64 字符的规范用户名。
	value string
}

// loginLimitCounter 保存一个维度在当前固定窗口内的失败数与尚未结束的预留。
type loginLimitCounter struct {
	// failures 是当前窗口已结算的错误凭证次数。
	failures int
	// inFlight 是已放行但尚未结算的请求数，跨窗口仍保留。
	inFlight int
	// expiresAt 是当前 60 秒窗口的截止时间。
	expiresAt time.Time
}

// LoginFailureLimiter 是有界、并发安全的单进程登录失败限流器，不持久化计数。
type LoginFailureLimiter struct {
	// mutex 保护计数、清理和双维度预留；锁内不执行凭证查询或 bcrypt。
	mutex sync.Mutex
	// records 同时保存两个维度，条目数不超过 maxRecords。
	records map[loginLimitKey]*loginLimitCounter
	// maxRecords 是总条目数上限，构造时固定为 4096。
	maxRecords int
	// now 在锁内读取，默认 time.Now，测试可在使用前替换。
	now func() time.Time
}

// NewLoginFailureLimiter 创建空的限流器，不启动清理 goroutine。
func NewLoginFailureLimiter() *LoginFailureLimiter {
	// 1. 初始化记录、容量及时间来源，清理在请求开始和完成时执行。
	return &LoginFailureLimiter{records: make(map[loginLimitKey]*loginLimitCounter), maxRecords: maxLoginLimitRecords, now: time.Now}
}

// Begin 同时检查并预留两个维度的名额；sourceIP 必须来自可信连接地址。
// 放行返回 complete/0/nil，受限返回 nil/建议等待时长/nil，无效参数返回 nil/0/error。
// 调用方必须结算：先 defer complete(false)，确定为错误凭证时调用 complete(true)。
// complete 幂等，第一次调用决定结果；成功、取消、内部故障均使用 false。
func (limiter *LoginFailureLimiter) Begin(sourceIP netip.Addr, username string) (complete func(bool), retryAfter time.Duration, err error) {
	// 1. 校验规范输入，在锁内清理过期记录。
	if !sourceIP.IsValid() || sourceIP.Zone() != "" || !loginLimitUsernamePattern.MatchString(username) {
		return nil, 0, fmt.Errorf("登录限流需要有效的无区域 IP 和规范用户名")
	}
	keys := [2]loginLimitKey{{scope: "ip", value: sourceIP.Unmap().String()}, {scope: "username", value: username}}
	limiter.mutex.Lock()
	defer limiter.mutex.Unlock()
	now := limiter.now()
	limiter.expireLocked(now)

	// 2. 检查两个维度的失败数与在途预留，并检查整体容量。
	missingRecords := 0
	for _, key := range keys {
		counter := limiter.records[key]
		if counter == nil {
			missingRecords++
		} else if counter.failures+counter.inFlight >= loginFailureLimit {
			if remaining := counter.expiresAt.Sub(now); remaining > retryAfter {
				retryAfter = remaining
			}
		}
	}
	if retryAfter > 0 {
		return nil, retryAfter, nil
	}
	if len(limiter.records)+missingRecords > limiter.maxRecords {
		return nil, limiter.capacityRetryLocked(now), nil
	}

	// 3. 原子预留两个名额，返回结束时的幂等结算回调。
	var counters [2]*loginLimitCounter
	for index, key := range keys {
		counter := limiter.records[key]
		if counter == nil {
			counter = &loginLimitCounter{expiresAt: now.Add(loginFailureWindow)}
			limiter.records[key] = counter
		}
		counter.inFlight++
		counters[index] = counter
	}
	var once sync.Once
	complete = func(failed bool) {
		// 4. 只结算一次；过期保留在途，完成后才释放没有失败的空记录。
		once.Do(func() {
			limiter.mutex.Lock()
			defer limiter.mutex.Unlock()
			limiter.expireLocked(limiter.now())
			for index, counter := range counters {
				counter.inFlight--
				if failed {
					counter.failures++
				}
				if counter.failures == 0 && counter.inFlight == 0 {
					delete(limiter.records, keys[index])
				}
			}
		})
	}
	return complete, 0, nil
}

// expireLocked 在持有 mutex 时清理失败窗口；不删除仍被完成回调引用的在途记录。
func (limiter *LoginFailureLimiter) expireLocked(now time.Time) {
	// 1. 过期且没有在途请求时直接删除；仍有在途时只清失败数并开启新窗口。
	for key, counter := range limiter.records {
		if !now.Before(counter.expiresAt) {
			if counter.inFlight == 0 {
				delete(limiter.records, key)
			} else {
				counter.failures = 0
				counter.expiresAt = now.Add(loginFailureWindow)
			}
		}
	}
}

// capacityRetryLocked 在持有 mutex 时为总容量受限提供最早窗口的等待建议。
func (limiter *LoginFailureLimiter) capacityRetryLocked(now time.Time) time.Duration {
	// 1. 记录均已清理过期状态，选择最早到期值；在途请求未结束时不保证到期即放行。
	retryAfter := loginFailureWindow
	for _, counter := range limiter.records {
		if remaining := counter.expiresAt.Sub(now); remaining < retryAfter {
			retryAfter = remaining
		}
	}
	return retryAfter
}
