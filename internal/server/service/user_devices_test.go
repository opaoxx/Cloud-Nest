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

func userDeviceServiceFixture() (domain.Actor, domain.UserDevicePage) {
	createdAt := time.Date(2026, 11, 13, 3, 0, 0, 123456000, time.UTC)
	current := domain.UserDevice{ID: 13, UserID: 7, Name: "学习设备 📁", Platform: "WEB", CreatedAt: createdAt}
	newer, older := current, current
	newer.ID, older.ID = 17, 11
	return domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}, domain.UserDevicePage{Devices: []domain.UserDevice{newer, current, older}}
}

func TestUserDeviceServiceResults(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		change      func(*domain.Actor, *domain.UserDevicePage, *uint64, *int)
		wantCurrent []bool
	}{
		{name: "current follows device id", wantCurrent: []bool{false, true, false}},
		{name: "another session on same device", change: func(actor *domain.Actor, _ *domain.UserDevicePage, _ *uint64, _ *int) { actor.SessionID = 17 }, wantCurrent: []bool{false, true, false}},
		{name: "different current device", change: func(actor *domain.Actor, _ *domain.UserDevicePage, _ *uint64, _ *int) { actor.DeviceID = 17 }, wantCurrent: []bool{true, false, false}},
		{name: "current absent from page", change: func(actor *domain.Actor, page *domain.UserDevicePage, _ *uint64, _ *int) {
			actor.DeviceID = 9
			page.HasMore = true
		}, wantCurrent: []bool{false, false, false}},
		{name: "current on later page", change: func(_ *domain.Actor, page *domain.UserDevicePage, beforeID *uint64, size *int) {
			*beforeID, *size = 17, 2
			page.Devices = page.Devices[1:]
		}, wantCurrent: []bool{true, false}},
		{name: "empty initialized page", change: func(_ *domain.Actor, page *domain.UserDevicePage, _ *uint64, _ *int) {
			page.Devices = []domain.UserDevice{}
		}, wantCurrent: []bool{}},
		{name: "nil slice becomes empty list", change: func(_ *domain.Actor, page *domain.UserDevicePage, _ *uint64, _ *int) { page.Devices = nil }, wantCurrent: []bool{}},
		{name: "history and null facts preserved", change: func(_ *domain.Actor, page *domain.UserDevicePage, _ *uint64, _ *int) {
			seenAt := page.Devices[0].CreatedAt.Add(time.Minute)
			page.Devices[0].LastSeenAt, page.Devices[1].RevokedAt = &seenAt, &seenAt
		}, wantCurrent: []bool{false, true, false}},
		{name: "full unsigned identifiers", change: func(actor *domain.Actor, page *domain.UserDevicePage, _ *uint64, size *int) {
			*actor = domain.Actor{UserID: ^uint64(0), SessionID: ^uint64(0) - 1, DeviceID: ^uint64(0)}
			device := page.Devices[0]
			device.ID, device.UserID = actor.DeviceID, actor.UserID
			page.Devices, *size = []domain.UserDevice{device}, 1
		}, wantCurrent: []bool{true}},
		{name: "maximum cursor", change: func(_ *domain.Actor, _ *domain.UserDevicePage, beforeID *uint64, _ *int) { *beforeID = ^uint64(0) }, wantCurrent: []bool{false, true, false}},
		{name: "maximum page size", change: func(_ *domain.Actor, _ *domain.UserDevicePage, _ *uint64, size *int) { *size = 100 }, wantCurrent: []bool{false, true, false}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, page := userDeviceServiceFixture()
			beforeID, pageSize := uint64(0), 3
			if testCase.change != nil {
				testCase.change(&actor, &page, &beforeID, &pageSize)
			}
			type requestKey struct{}
			ctx := context.WithValue(context.Background(), requestKey{}, "fixture")
			calls := 0
			devices := NewUserDeviceService(func(received context.Context, userID, receivedBefore uint64, receivedSize int) (*domain.UserDevicePage, error) {
				calls++
				if received != ctx || userID != actor.UserID || receivedBefore != beforeID || receivedSize != pageSize {
					t.Error("设备查询未收到原上下文和准确参数")
				}
				return &page, nil
			})
			result, err := devices.List(ctx, actor, beforeID, pageSize)
			if err != nil || result == nil || result.Devices == nil || len(result.Devices) != len(testCase.wantCurrent) || result.HasMore != page.HasMore || calls != 1 {
				t.Fatal("设备列表结果、分页或调用次数错误", err)
			}
			for index, item := range result.Devices {
				if item.Current != testCase.wantCurrent[index] || !reflect.DeepEqual(item.UserDevice, page.Devices[index]) {
					t.Fatal("当前设备标记或原始事实映射错误")
				}
			}
		})
	}
}

func TestUserDeviceServiceRejectsInvalidResults(t *testing.T) {
	failure := errors.New("fixture query failure")
	for _, testCase := range []struct {
		name             string
		change           func(*domain.UserDevicePage, *uint64, *int)
		nilPage          bool
		queryError, want error
	}{
		{name: "nil page", nilPage: true, want: domain.ErrInvalidDeviceData},
		{name: "oversized page", change: func(_ *domain.UserDevicePage, _ *uint64, size *int) { *size = 2 }, want: domain.ErrInvalidDeviceData},
		{name: "short page with more", change: func(page *domain.UserDevicePage, _ *uint64, size *int) { page.HasMore, *size = true, 4 }, want: domain.ErrInvalidDeviceData},
		{name: "empty page with more", change: func(page *domain.UserDevicePage, _ *uint64, _ *int) { page.Devices, page.HasMore = nil, true }, want: domain.ErrInvalidDeviceData},
		{name: "wrong owner", change: func(page *domain.UserDevicePage, _ *uint64, _ *int) { page.Devices[1].UserID = 99 }, want: domain.ErrInvalidDeviceData},
		{name: "zero device", change: func(page *domain.UserDevicePage, _ *uint64, _ *int) { page.Devices[2].ID = 0 }, want: domain.ErrInvalidDeviceData},
		{name: "duplicate device", change: func(page *domain.UserDevicePage, _ *uint64, _ *int) { page.Devices[1].ID = page.Devices[0].ID }, want: domain.ErrInvalidDeviceData},
		{name: "ascending order", change: func(page *domain.UserDevicePage, _ *uint64, _ *int) {
			page.Devices[0], page.Devices[1] = page.Devices[1], page.Devices[0]
		}, want: domain.ErrInvalidDeviceData},
		{name: "device equals cursor", change: func(_ *domain.UserDevicePage, beforeID *uint64, _ *int) { *beforeID = 17 }, want: domain.ErrInvalidDeviceData},
		{name: "device exceeds cursor", change: func(_ *domain.UserDevicePage, beforeID *uint64, _ *int) { *beforeID = 16 }, want: domain.ErrInvalidDeviceData},
		{name: "query failure", queryError: failure, want: failure},
		{name: "wrapped query failure", queryError: fmt.Errorf("fixture wrapped: %w", failure), want: failure},
		{name: "repository data error", queryError: domain.ErrInvalidDeviceData, want: domain.ErrInvalidDeviceData},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, page := userDeviceServiceFixture()
			beforeID, pageSize := uint64(0), 3
			if testCase.change != nil {
				testCase.change(&page, &beforeID, &pageSize)
			}
			calls := 0
			devices := NewUserDeviceService(func(context.Context, uint64, uint64, int) (*domain.UserDevicePage, error) {
				calls++
				if testCase.nilPage {
					return nil, testCase.queryError
				}
				return &page, testCase.queryError
			})
			result, err := devices.List(context.Background(), actor, beforeID, pageSize)
			if result != nil || !errors.Is(err, testCase.want) || calls != 1 {
				t.Fatal("异常页面返回了结果或丢失错误原因", err)
			}
		})
	}
}

func TestUserDeviceServiceCopies(t *testing.T) {
	actor, page := userDeviceServiceFixture()
	seenAt := page.Devices[0].CreatedAt.Add(time.Minute)
	page.Devices[0].LastSeenAt, page.Devices[0].RevokedAt = &seenAt, &seenAt
	devices := NewUserDeviceService(func(context.Context, uint64, uint64, int) (*domain.UserDevicePage, error) { return &page, nil })
	first, err := devices.List(context.Background(), actor, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := devices.List(context.Background(), actor, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first.Devices[0].LastSeenAt == page.Devices[0].LastSeenAt || first.Devices[0].RevokedAt == page.Devices[0].RevokedAt ||
		first.Devices[0].LastSeenAt == first.Devices[0].RevokedAt || first.Devices[0].LastSeenAt == second.Devices[0].LastSeenAt {
		t.Fatal("返回列表或可空时间共享了可修改对象")
	}
	*first.Devices[0].LastSeenAt = seenAt.Add(time.Hour)
	*first.Devices[0].RevokedAt = seenAt.Add(2 * time.Hour)
	first.Devices[0].Name, first.Devices[1].Current, first.HasMore = "修改副本", false, true
	if !page.Devices[0].LastSeenAt.Equal(seenAt) || !page.Devices[0].RevokedAt.Equal(seenAt) ||
		!reflect.DeepEqual(second.Devices[0].UserDevice, page.Devices[0]) || page.HasMore || second.HasMore || !second.Devices[1].Current {
		t.Fatal("修改结果影响了查询对象或另一次结果")
	}
	page.Devices[1].Name = "修改来源"
	if second.Devices[1].Name == "修改来源" {
		t.Fatal("查询来源与返回切片相互影响")
	}
}

func TestUserDeviceServiceInputAndCancellation(t *testing.T) {
	for _, phase := range []string{"nil context", "zero user", "zero session", "zero device", "zero page size", "negative page size", "excessive page size", "before query", "after query", "deadline", "query error with cancellation", "before return"} {
		t.Run(phase, func(t *testing.T) {
			actor, page := userDeviceServiceFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pageSize, expectedCalls := 3, 0
			var want error
			switch phase {
			case "nil context":
				ctx = nil
			case "zero user":
				actor.UserID = 0
			case "zero session":
				actor.SessionID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero page size":
				pageSize = 0
			case "negative page size":
				pageSize = -1
			case "excessive page size":
				pageSize = 101
			case "before query":
				cancel()
				want = context.Canceled
			case "after query", "query error with cancellation", "before return":
				expectedCalls, want = 1, context.Canceled
			case "deadline":
				var release context.CancelFunc
				ctx, release = context.WithTimeout(ctx, 20*time.Millisecond)
				defer release()
				expectedCalls, want = 1, context.DeadlineExceeded
			}
			if phase == "before return" {
				// 复用已有上下文，在列表复制完成后的第三次检查时取消。
				ctx = &cancelBeforeSessionListReturnContext{Context: ctx, cancel: cancel}
			}
			calls := 0
			devices := NewUserDeviceService(func(received context.Context, _ uint64, _ uint64, _ int) (*domain.UserDevicePage, error) {
				calls++
				if phase == "deadline" {
					<-received.Done()
					return nil, received.Err()
				}
				if phase != "before return" {
					cancel()
				}
				if phase == "query error with cancellation" {
					return &page, errors.New("fixture concurrent failure")
				}
				return &page, nil
			})
			result, err := devices.List(ctx, actor, 0, pageSize)
			if result != nil || err == nil || calls != expectedCalls || (want != nil && !errors.Is(err, want)) {
				t.Fatal("参数或取消未按预期结束处理", err)
			}
		})
	}
}

func TestUserDeviceServiceDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少设备查询仍构造成功")
		}
	}()
	NewUserDeviceService(nil)
}
