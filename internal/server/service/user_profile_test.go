package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUserProfileServiceResults(t *testing.T) {
	base := domain.UserProfile{ID: 7, Username: "fixture", DisplayName: "学习用户 🌱", Status: "ACTIVE", RootEntryID: 13, QuotaBytes: 100, UsedBytes: 60, ReservedBytes: 40}
	disabled, unknown, wrongOwner, zeroRoot, large, zeroQuota := base, base, base, base, base, base
	disabled.Status, unknown.Status = "DISABLED", "UNKNOWN"
	wrongOwner.ID, zeroRoot.RootEntryID = 99, 0
	large.ID, large.RootEntryID, large.QuotaBytes, large.UsedBytes, large.ReservedBytes = ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), 0
	zeroQuota.QuotaBytes, zeroQuota.UsedBytes, zeroQuota.ReservedBytes = 0, 0, 0
	fault := errors.New("fixture database failure")
	for _, testCase := range []struct {
		name    string
		userID  uint64
		profile *domain.UserProfile
		err     error
		want    *domain.UserProfile
		wantErr error
	}{
		{name: "active", userID: 7, profile: &base, want: &base},
		{name: "full uint64", userID: ^uint64(0), profile: &large, want: &large},
		{name: "zero quota", userID: 7, profile: &zeroQuota, want: &zeroQuota},
		{name: "disabled", userID: 7, profile: &disabled, wantErr: domain.ErrAccountDisabled},
		{name: "unknown status", userID: 7, profile: &unknown, wantErr: domain.ErrInvalidAccountData},
		{name: "wrong owner", userID: 7, profile: &wrongOwner, wantErr: domain.ErrInvalidAccountData},
		{name: "missing root", userID: 7, profile: &zeroRoot, wantErr: domain.ErrInvalidAccountData},
		{name: "nil profile", userID: 7, wantErr: domain.ErrInvalidAccountData},
		{name: "not found", userID: 7, err: domain.ErrAccountNotFound, wantErr: domain.ErrAccountNotFound},
		{name: "wrapped not found", userID: 7, err: fmt.Errorf("fixture wrapped: %w", domain.ErrAccountNotFound), wantErr: domain.ErrAccountNotFound},
		{name: "query failure", userID: 7, err: fault, wantErr: fault},
		{name: "repository data error", userID: 7, err: domain.ErrInvalidAccountData, wantErr: domain.ErrInvalidAccountData},
		{name: "error with partial result", userID: 7, profile: &base, err: fault, wantErr: fault},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			calls := 0
			profileService := NewUserProfileService(func(received context.Context, userID uint64) (*domain.UserProfile, error) {
				calls++
				if received != ctx || userID != testCase.userID {
					t.Error("查询未收到原上下文与用户 ID")
				}
				return testCase.profile, testCase.err
			})
			var before domain.UserProfile
			if testCase.profile != nil {
				before = *testCase.profile
			}
			result, err := profileService.Get(ctx, testCase.userID)
			if calls != 1 {
				t.Fatal("每次请求应恰好查询一次资料")
			}
			if testCase.want != nil {
				if err != nil || !reflect.DeepEqual(result, testCase.want) || result == testCase.profile {
					t.Fatal("资料未原样返回或持有查询结果原指针")
				}
				result.DisplayName = "修改读取副本"
			} else if result != nil || !errors.Is(err, testCase.wantErr) {
				t.Fatal("拒绝结果错误或丢失错误原因")
			}
			if testCase.profile != nil && *testCase.profile != before {
				t.Fatal("服务修改了查询返回的资料对象")
			}
			if testCase.wantErr == domain.ErrAccountNotFound && err != domain.ErrAccountNotFound {
				t.Fatal("账号未找到应返回约定领域错误")
			}
		})
	}
}

func TestUserProfileServiceInputAndCancellation(t *testing.T) {
	for _, phase := range []string{"nil context", "zero user", "before query", "after query", "deadline"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			userID, expectedCalls := uint64(7), 0
			var want error
			switch phase {
			case "nil context":
				ctx = nil
			case "zero user":
				userID = 0
			case "before query":
				cancel()
				want = context.Canceled
			case "after query":
				expectedCalls, want = 1, context.Canceled
			case "deadline":
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 20*time.Millisecond)
				defer stop()
				expectedCalls, want = 1, context.DeadlineExceeded
			}
			calls := 0
			profileService := NewUserProfileService(func(ctx context.Context, _ uint64) (*domain.UserProfile, error) {
				calls++
				if phase == "deadline" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				cancel()
				return &domain.UserProfile{ID: 7, Status: "ACTIVE", RootEntryID: 13}, nil
			})
			result, err := profileService.Get(ctx, userID)
			if result != nil || err == nil || calls != expectedCalls || want != nil && !errors.Is(err, want) {
				t.Fatal("错误输入或取消未按阶段结束处理")
			}
		})
	}
}

func TestUserProfileServiceDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少查询方法应暴露装配错误")
		}
	}()
	NewUserProfileService(nil)
}
