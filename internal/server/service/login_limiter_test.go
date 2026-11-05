package service

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// requireLoginSlot 只供测试：预期准入失败时停止当前用例，不继续使用 nil 回调。
func requireLoginSlot(t *testing.T, limiter *LoginFailureLimiter, address netip.Addr, username string) func(bool) {
	t.Helper()
	complete, retryAfter, err := limiter.Begin(address, username)
	if err != nil || complete == nil || retryAfter != 0 {
		t.Fatalf("预期放行 retry=%s error=%v", retryAfter, err)
	}
	return complete
}

func TestLoginFailureLimiterWindowAndSettlement(t *testing.T) {
	// 1. 不等待真实一分钟，检查第 10 次、拒绝不续期及精确到期边界。
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	limiter := NewLoginFailureLimiter()
	limiter.now = func() time.Time { return now }
	address := netip.MustParseAddr("192.0.2.1")
	for range 10 {
		requireLoginSlot(t, limiter, address, "root")(true)
	}
	for _, remaining := range []time.Duration{45 * time.Second, 30 * time.Second, time.Nanosecond} {
		now = time.Date(2026, 11, 5, 10, 1, 0, 0, time.UTC).Add(-remaining)
		if complete, retry, err := limiter.Begin(address, "root"); complete != nil || retry != remaining || err != nil {
			t.Fatalf("拒绝续期或边界错误 retry=%s err=%v", retry, err)
		}
	}
	now = now.Add(time.Nanosecond)
	requireLoginSlot(t, limiter, address, "root")(false)
	if len(limiter.records) != 0 {
		t.Fatal("到期旧记录或无失败的空记录未释放")
	}

	// 2. 成功只释放预留，不能抹掉同一窗口中已有的失败。
	requireLoginSlot(t, limiter, address, "root")(true)
	requireLoginSlot(t, limiter, address, "root")(false)
	for range 9 {
		requireLoginSlot(t, limiter, address, "root")(true)
	}
	if complete, retry, err := limiter.Begin(address, "root"); complete != nil || retry <= 0 || err != nil {
		t.Fatal("成功请求清除了已有失败")
	}
}

func TestLoginFailureLimiterIndependentDimensions(t *testing.T) {
	for _, dimension := range []string{"ip", "username", "mapped ipv4"} {
		t.Run(dimension, func(t *testing.T) {
			limiter := NewLoginFailureLimiter()
			address := netip.MustParseAddr("192.0.2.1")
			for attemptIndex := range 10 {
				username := "root"
				if dimension == "username" {
					address = netip.AddrFrom4([4]byte{192, 0, 2, byte(attemptIndex + 1)})
				} else {
					username = fmt.Sprintf("user_%d", attemptIndex)
				}
				if dimension == "mapped ipv4" {
					address = netip.MustParseAddr("::ffff:192.0.2.1")
				}
				requireLoginSlot(t, limiter, address, username)(true)
			}
			blockedAddress, blockedUsername := netip.MustParseAddr("192.0.2.1"), "fresh_user"
			if dimension == "username" {
				blockedAddress, blockedUsername = netip.MustParseAddr("192.0.2.99"), "root"
			}
			if complete, retry, err := limiter.Begin(blockedAddress, blockedUsername); complete != nil || retry <= 0 || err != nil {
				t.Fatal("只限制了 IP/用户名配对，或映射 IP 绕过计数")
			}
			requireLoginSlot(t, limiter, netip.MustParseAddr("198.51.100.1"), "unrelated_user")(false)
		})
	}
}

func TestLoginFailureLimiterUsesLaterBlockingWindow(t *testing.T) {
	// 两个阻塞维度有不同截止时间，建议等待应覆盖两者。
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	limiter := NewLoginFailureLimiter()
	limiter.now = func() time.Time { return now }
	firstAddress, secondAddress := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	for attemptIndex := range 10 {
		requireLoginSlot(t, limiter, firstAddress, fmt.Sprintf("user_%d", attemptIndex))(true)
	}
	now = now.Add(10 * time.Second)
	for range 10 {
		requireLoginSlot(t, limiter, secondAddress, "root")(true)
	}
	now = now.Add(10 * time.Second)
	if complete, retry, err := limiter.Begin(firstAddress, "root"); complete != nil || retry != 50*time.Second || err != nil {
		t.Fatalf("未选择较晚的阻塞窗口 retry=%s err=%v", retry, err)
	}
}

func TestLoginFailureLimiterInFlightAndExpiry(t *testing.T) {
	// 1. 在途名额不能因跨过窗口而被删除，也不能超过 10 个。
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	limiter := NewLoginFailureLimiter()
	limiter.now = func() time.Time { return now }
	address := netip.MustParseAddr("192.0.2.1")
	var completions []func(bool)
	for range 10 {
		completions = append(completions, requireLoginSlot(t, limiter, address, "root"))
	}
	now = now.Add(time.Minute)
	if complete, retry, err := limiter.Begin(address, "root"); complete != nil || retry != time.Minute || err != nil {
		t.Fatal("跨窗口清理丢失了在途预留")
	}
	completions[0](true)
	for _, complete := range completions[1:] {
		complete(false)
	}
	for _, counter := range limiter.records {
		if counter.failures != 1 || counter.inFlight != 0 {
			t.Fatal("跨窗口回调结算到失效对象或发生重复计数")
		}
	}
	requireLoginSlot(t, limiter, address, "root")(false)

	// 2. 下一窗口应清除旧失败，但仍保留尚未完成的请求。
	for range 8 {
		requireLoginSlot(t, limiter, address, "root")(true)
	}
	lastPending := requireLoginSlot(t, limiter, address, "root")
	now = now.Add(time.Minute)
	requireLoginSlot(t, limiter, address, "root")(false)
	lastPending(true)
	for _, counter := range limiter.records {
		if counter.failures != 1 || counter.inFlight != 0 {
			t.Fatal("旧窗口失败未清除或在途结算错误")
		}
	}
}

func TestLoginFailureLimiterCompletionIsIdempotent(t *testing.T) {
	limiter := NewLoginFailureLimiter()
	address := netip.MustParseAddr("192.0.2.1")
	complete := requireLoginSlot(t, limiter, address, "root")
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(func() { complete(true) })
	}
	workers.Wait()
	complete(false)
	for _, counter := range limiter.records {
		if counter.failures != 1 || counter.inFlight != 0 {
			t.Fatal("重复结算改变了计数")
		}
	}
	otherComplete := requireLoginSlot(t, limiter, netip.MustParseAddr("198.51.100.1"), "other")
	otherComplete(false)
	otherComplete(true)
	if len(limiter.records) != 2 {
		t.Fatal("第一次释放后，重复 true 又产生了失败记录")
	}
}

func TestLoginFailureLimiterCapacity(t *testing.T) {
	// 1. 只剩一条空间时不能部分插入两条；已有额度仍可使用，且不能驱逐未过期记录。
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	limiter := NewLoginFailureLimiter()
	if limiter.maxRecords != 4096 {
		t.Fatal("默认总容量不符合设计")
	}
	limiter.maxRecords = 3
	limiter.now = func() time.Time { return now }
	firstAddress, otherAddress := netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	requireLoginSlot(t, limiter, firstAddress, "root")(true)
	if complete, retry, err := limiter.Begin(otherAddress, "other"); complete != nil || retry != time.Minute || err != nil || len(limiter.records) != 2 {
		t.Fatal("容量不足时发生部分插入或驱逐")
	}
	requireLoginSlot(t, limiter, firstAddress, "other")(false)
	if len(limiter.records) != 2 {
		t.Fatal("释放空记录时删除了已有失败，或留下了无用条目")
	}
	now = now.Add(time.Minute)
	requireLoginSlot(t, limiter, otherAddress, "other")(false)
	if len(limiter.records) != 0 {
		t.Fatal("过期记录未释放")
	}

	// 2. 在途记录不能为新键腾位置而被驱逐；结束后容量可用。
	limiter.maxRecords = 2
	pending := requireLoginSlot(t, limiter, firstAddress, "root")
	now = now.Add(time.Minute)
	if complete, retry, err := limiter.Begin(otherAddress, "other"); complete != nil || retry <= 0 || err != nil || len(limiter.records) != 2 {
		t.Fatal("容量满时驱逐了在途记录")
	}
	pending(false)
	for range 20 {
		requireLoginSlot(t, limiter, otherAddress, "other")(false)
	}
	if len(limiter.records) != 0 {
		t.Fatal("无失败请求持续占用容量")
	}
}

func TestLoginFailureLimiterRejectsInvalidKeys(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		address  netip.Addr
		username string
	}{
		{name: "invalid ip", username: "root"},
		{name: "zoned ip", address: netip.MustParseAddr("fe80::1%private-zone"), username: "root"},
		{name: "empty username", address: netip.MustParseAddr("192.0.2.1")},
		{name: "long username", address: netip.MustParseAddr("192.0.2.1"), username: strings.Repeat("a", 65)},
		{name: "uppercase username", address: netip.MustParseAddr("192.0.2.1"), username: "ROOT"},
		{name: "non ascii username", address: netip.MustParseAddr("192.0.2.1"), username: "用户"},
		{name: "punctuation username", address: netip.MustParseAddr("192.0.2.1"), username: "private-user"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			limiter := NewLoginFailureLimiter()
			complete, retry, err := limiter.Begin(testCase.address, testCase.username)
			if complete != nil || retry != 0 || err == nil || len(limiter.records) != 0 {
				t.Fatal("非法键被接受或污染记录")
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatal("错误回显原始键")
			}
		})
	}
}

func TestLoginFailureLimiterCapacityWaitAndValidKeys(t *testing.T) {
	// 1. 容量拒绝使用最早到期记录，不应误用双维度额度拒绝的较晚窗口。
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	limiter := NewLoginFailureLimiter()
	limiter.maxRecords = 4
	limiter.now = func() time.Time { return now }
	requireLoginSlot(t, limiter, netip.MustParseAddr("2001:db8::1"), "a")(true)
	now = now.Add(10 * time.Second)
	requireLoginSlot(t, limiter, netip.MustParseAddr("2001:db8::2"), strings.Repeat("a", 64))(true)
	now = now.Add(10 * time.Second)
	if complete, retry, err := limiter.Begin(netip.MustParseAddr("2001:db8::3"), "other"); complete != nil || retry != 40*time.Second || err != nil {
		t.Fatalf("容量等待没有参考最早窗口 retry=%s error=%v", retry, err)
	}
}

func TestLoginFailureLimiterConcurrentAdmission(t *testing.T) {
	for _, dimension := range []string{"same pair", "same ip", "same username", "capacity"} {
		t.Run(dimension, func(t *testing.T) {
			limiter := NewLoginFailureLimiter()
			if dimension == "capacity" {
				limiter.maxRecords = 6
			}
			type outcome struct {
				complete func(bool)
				retry    time.Duration
				err      error
			}
			outcomes := make(chan outcome, 64)
			start := make(chan struct{})
			for requestIndex := range 64 {
				go func(requestIndex int) {
					<-start
					address, username := netip.MustParseAddr("192.0.2.1"), "root"
					if dimension == "same username" || dimension == "capacity" {
						address = netip.AddrFrom4([4]byte{192, 0, 2, byte(requestIndex + 1)})
					}
					if dimension == "same ip" || dimension == "capacity" {
						username = fmt.Sprintf("user_%d", requestIndex)
					}
					complete, retry, err := limiter.Begin(address, username)
					outcomes <- outcome{complete: complete, retry: retry, err: err}
				}(requestIndex)
			}
			close(start)
			var admitted []func(bool)
			for range 64 {
				result := <-outcomes
				if result.err != nil {
					t.Fatal(result.err)
				}
				if result.complete != nil {
					if result.retry != 0 {
						t.Fatal("放行与等待同时出现")
					}
					admitted = append(admitted, result.complete)
				} else if result.retry <= 0 {
					t.Fatal("受限没有等待建议")
				}
			}
			want := 10
			if dimension == "capacity" {
				want = 3
			}
			if len(admitted) != want || len(limiter.records) > limiter.maxRecords {
				t.Fatalf("并发突破额度/容量 admitted=%d records=%d", len(admitted), len(limiter.records))
			}
			var workers sync.WaitGroup
			for _, complete := range admitted {
				workers.Go(func() { complete(false) })
			}
			workers.Wait()
			if len(limiter.records) != 0 {
				t.Fatal("并发释放后残留空记录")
			}
		})
	}
}

func TestLoginFailureLimiterCredentialFlow(t *testing.T) {
	// 1. 演示只将错误凭证计数；成功、取消、禁用正确密码和内部故障均释放预留。
	limiter := NewLoginFailureLimiter()
	address := netip.MustParseAddr("192.0.2.1")
	queryCalls := 0
	var queryError error
	status := "ACTIVE"
	credentialService := NewCredentialService(func(context.Context, string) (*domain.LoginAccount, error) {
		queryCalls++
		if queryError != nil {
			return nil, queryError
		}
		return &domain.LoginAccount{ID: 1, PasswordHash: "private-test-hash", Status: status}, nil
	})
	credentialService.verifyPassword = func(_ string, password string) (bool, error) { return password == "root", nil }
	limited := errors.New("测试限流结果")
	runAttempt := func(ctx context.Context, password string) error {
		complete, _, err := limiter.Begin(address, "root")
		if err != nil {
			return err
		}
		if complete == nil {
			return limited
		}
		defer complete(false)
		_, credentialError := credentialService.Authenticate(ctx, "root", password)
		complete(errors.Is(credentialError, domain.ErrInvalidCredentials))
		return credentialError
	}
	for range 9 {
		if err := runAttempt(context.Background(), "wrong"); err != domain.ErrInvalidCredentials {
			t.Fatal("错误凭证流程失败")
		}
	}
	if err := runAttempt(context.Background(), "root"); err != nil {
		t.Fatal("成功流程失败")
	}
	queryError = errors.New("database unavailable")
	if err := runAttempt(context.Background(), "root"); !errors.Is(err, queryError) {
		t.Fatal("内部故障分类丢失")
	}
	queryError, status = nil, "DISABLED"
	if err := runAttempt(context.Background(), "root"); err != domain.ErrAccountDisabled {
		t.Fatal("禁用状态分类丢失")
	}
	status = "ACTIVE"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runAttempt(ctx, "root"); !errors.Is(err, context.Canceled) {
		t.Fatal("取消流程失败")
	}
	if err := runAttempt(context.Background(), "wrong"); err != domain.ErrInvalidCredentials {
		t.Fatal("非凭证失败错误消耗了额度")
	}
	before := queryCalls
	if err := runAttempt(context.Background(), "root"); err != limited || queryCalls != before {
		t.Fatal("阈值后仍执行了查询")
	}
}
