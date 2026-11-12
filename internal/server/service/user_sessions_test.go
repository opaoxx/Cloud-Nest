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

func userSessionServiceFixture() (domain.Actor, domain.UserSessionPage) {
	createdAt := time.Date(2026, 11, 12, 3, 0, 0, 123456000, time.UTC)
	current := domain.UserSession{ID: 11, UserID: 7, DeviceID: 13, DeviceName: "学习设备 📁", DevicePlatform: "WEB", CreatedAt: createdAt, ExpiresAt: createdAt.Add(time.Hour)}
	newer, older := current, current
	newer.ID, older.ID = 15, 9
	return domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}, domain.UserSessionPage{Sessions: []domain.UserSession{newer, current, older}}
}

func TestUserSessionServiceResults(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		change      func(*domain.Actor, *domain.UserSessionPage, *uint64, *int)
		wantCurrent []bool
	}{
		{name: "same device has one current session", wantCurrent: []bool{false, true, false}},
		{name: "multiple devices in page", change: func(_ *domain.Actor, page *domain.UserSessionPage, _ *uint64, _ *int) { page.Sessions[0].DeviceID = 17 }, wantCurrent: []bool{false, true, false}},
		{name: "current absent from page", change: func(actor *domain.Actor, page *domain.UserSessionPage, _ *uint64, _ *int) {
			actor.SessionID = 7
			page.HasMore = true
		}, wantCurrent: []bool{false, false, false}},
		{name: "current on later page", change: func(_ *domain.Actor, page *domain.UserSessionPage, beforeID *uint64, pageSize *int) {
			*beforeID, *pageSize = 15, 2
			page.Sessions = page.Sessions[1:]
		}, wantCurrent: []bool{true, false}},
		{name: "empty initialized page", change: func(_ *domain.Actor, page *domain.UserSessionPage, _ *uint64, _ *int) {
			page.Sessions = []domain.UserSession{}
		}, wantCurrent: []bool{}},
		{name: "nil slice becomes initialized empty list", change: func(_ *domain.Actor, page *domain.UserSessionPage, _ *uint64, _ *int) { page.Sessions = nil }, wantCurrent: []bool{}},
		{name: "history and nullable times preserved", change: func(_ *domain.Actor, page *domain.UserSessionPage, _ *uint64, _ *int) {
			seenAt := page.Sessions[0].CreatedAt.Add(time.Minute)
			page.Sessions[0].LastSeenAt, page.Sessions[1].RevokedAt, page.Sessions[2].DeviceRevokedAt = &seenAt, &seenAt, &seenAt
			page.Sessions[2].ExpiresAt = page.Sessions[2].CreatedAt.Add(-time.Hour)
		}, wantCurrent: []bool{false, true, false}},
		{name: "full unsigned identifiers", change: func(actor *domain.Actor, page *domain.UserSessionPage, _ *uint64, pageSize *int) {
			*actor = domain.Actor{UserID: ^uint64(0), SessionID: ^uint64(0), DeviceID: ^uint64(0)}
			session := page.Sessions[0]
			session.ID, session.UserID, session.DeviceID = actor.SessionID, actor.UserID, actor.DeviceID
			page.Sessions, *pageSize = []domain.UserSession{session}, 1
		}, wantCurrent: []bool{true}},
		{name: "full unsigned cursor", change: func(_ *domain.Actor, _ *domain.UserSessionPage, beforeID *uint64, _ *int) { *beforeID = ^uint64(0) }, wantCurrent: []bool{false, true, false}},
		{name: "maximum page size", change: func(_ *domain.Actor, _ *domain.UserSessionPage, _ *uint64, pageSize *int) { *pageSize = 100 }, wantCurrent: []bool{false, true, false}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, page := userSessionServiceFixture()
			beforeID, pageSize := uint64(0), 3
			if testCase.change != nil {
				testCase.change(&actor, &page, &beforeID, &pageSize)
			}
			ctx := context.WithValue(context.Background(), struct{}{}, "fixture context")
			calls := 0
			sessions := NewUserSessionService(func(received context.Context, userID, receivedBefore uint64, receivedSize int) (*domain.UserSessionPage, error) {
				calls++
				if received != ctx || userID != actor.UserID || receivedBefore != beforeID || receivedSize != pageSize {
					t.Error("列表查询未收到原上下文或准确参数")
				}
				return &page, nil
			})
			result, err := sessions.List(ctx, actor, beforeID, pageSize)
			if err != nil || result == nil || result.Sessions == nil || len(result.Sessions) != len(testCase.wantCurrent) || result.HasMore != page.HasMore || calls != 1 {
				t.Fatal("列表结果、分页或调用次数错误", err)
			}
			for index, item := range result.Sessions {
				if item.Current != testCase.wantCurrent[index] || !reflect.DeepEqual(item.UserSession, page.Sessions[index]) {
					t.Fatal("当前标记或原始事实映射错误")
				}
			}
		})
	}
}

func TestUserSessionServiceRejectsInvalidResults(t *testing.T) {
	failure := errors.New("fixture query failure")
	for _, testCase := range []struct {
		name       string
		change     func(*domain.UserSessionPage, *uint64, *int)
		nilPage    bool
		queryError error
		want       error
	}{
		{name: "nil page", nilPage: true, want: domain.ErrInvalidSessionData},
		{name: "oversized page", change: func(_ *domain.UserSessionPage, _ *uint64, size *int) { *size = 2 }, want: domain.ErrInvalidSessionData},
		{name: "short page with more", change: func(page *domain.UserSessionPage, _ *uint64, size *int) {
			page.HasMore = true
			*size = 4
		}, want: domain.ErrInvalidSessionData},
		{name: "empty page with more", change: func(page *domain.UserSessionPage, _ *uint64, _ *int) {
			page.Sessions = nil
			page.HasMore = true
		}, want: domain.ErrInvalidSessionData},
		{name: "wrong owner", change: func(page *domain.UserSessionPage, _ *uint64, _ *int) { page.Sessions[1].UserID = 99 }, want: domain.ErrInvalidSessionData},
		{name: "zero session", change: func(page *domain.UserSessionPage, _ *uint64, _ *int) { page.Sessions[2].ID = 0 }, want: domain.ErrInvalidSessionData},
		{name: "zero device", change: func(page *domain.UserSessionPage, _ *uint64, _ *int) { page.Sessions[2].DeviceID = 0 }, want: domain.ErrInvalidSessionData},
		{name: "duplicate session", change: func(page *domain.UserSessionPage, _ *uint64, _ *int) { page.Sessions[1].ID = page.Sessions[0].ID }, want: domain.ErrInvalidSessionData},
		{name: "ascending order", change: func(page *domain.UserSessionPage, _ *uint64, _ *int) {
			page.Sessions[0], page.Sessions[1] = page.Sessions[1], page.Sessions[0]
		}, want: domain.ErrInvalidSessionData},
		{name: "record equals cursor", change: func(_ *domain.UserSessionPage, beforeID *uint64, _ *int) { *beforeID = 15 }, want: domain.ErrInvalidSessionData},
		{name: "record exceeds cursor", change: func(_ *domain.UserSessionPage, beforeID *uint64, _ *int) { *beforeID = 14 }, want: domain.ErrInvalidSessionData},
		{name: "current device mismatch", change: func(page *domain.UserSessionPage, _ *uint64, _ *int) { page.Sessions[1].DeviceID = 17 }, want: domain.ErrInvalidSessionData},
		{name: "query failure", queryError: failure, want: failure},
		{name: "wrapped query failure", queryError: fmt.Errorf("fixture wrapped: %w", failure), want: failure},
		{name: "repository data error", queryError: domain.ErrInvalidSessionData, want: domain.ErrInvalidSessionData},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, page := userSessionServiceFixture()
			beforeID, pageSize := uint64(0), 3
			if testCase.change != nil {
				testCase.change(&page, &beforeID, &pageSize)
			}
			calls := 0
			sessions := NewUserSessionService(func(context.Context, uint64, uint64, int) (*domain.UserSessionPage, error) {
				calls++
				if testCase.nilPage {
					return nil, testCase.queryError
				}
				return &page, testCase.queryError
			})
			result, err := sessions.List(context.Background(), actor, beforeID, pageSize)
			if result != nil || !errors.Is(err, testCase.want) || calls != 1 {
				t.Fatal("异常页面返回了结果或丢失错误原因", err)
			}
		})
	}
}

func TestUserSessionServiceCopies(t *testing.T) {
	actor, page := userSessionServiceFixture()
	seenAt := page.Sessions[0].CreatedAt.Add(time.Minute)
	page.Sessions[0].LastSeenAt, page.Sessions[0].RevokedAt, page.Sessions[0].DeviceRevokedAt = &seenAt, &seenAt, &seenAt
	sessions := NewUserSessionService(func(context.Context, uint64, uint64, int) (*domain.UserSessionPage, error) { return &page, nil })
	first, err := sessions.List(context.Background(), actor, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessions.List(context.Background(), actor, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first.Sessions[0].LastSeenAt == page.Sessions[0].LastSeenAt || first.Sessions[0].RevokedAt == page.Sessions[0].RevokedAt ||
		first.Sessions[0].DeviceRevokedAt == page.Sessions[0].DeviceRevokedAt || first.Sessions[0].LastSeenAt == first.Sessions[0].RevokedAt {
		t.Fatal("列表或可空时间仍共享可修改对象")
	}
	*first.Sessions[0].LastSeenAt = seenAt.Add(time.Hour)
	*first.Sessions[0].RevokedAt = seenAt.Add(2 * time.Hour)
	*first.Sessions[0].DeviceRevokedAt = seenAt.Add(3 * time.Hour)
	first.Sessions[0].DeviceName, first.Sessions[1].Current, first.HasMore = "修改副本", false, true
	if !page.Sessions[0].LastSeenAt.Equal(seenAt) || !page.Sessions[0].RevokedAt.Equal(seenAt) || !page.Sessions[0].DeviceRevokedAt.Equal(seenAt) ||
		!reflect.DeepEqual(second.Sessions[0].UserSession, page.Sessions[0]) || page.Sessions[0].DeviceName == "修改副本" || page.HasMore || second.HasMore || !second.Sessions[1].Current {
		t.Fatal("修改结果影响了查询对象或另一次结果")
	}
	page.Sessions[1].DeviceName = "修改来源"
	if second.Sessions[1].DeviceName == "修改来源" {
		t.Fatal("查询来源与结果切片仍相互影响")
	}
}

func TestUserSessionServiceInputAndCancellation(t *testing.T) {
	for _, phase := range []string{"nil context", "zero user", "zero session", "zero device", "zero page size", "negative page size", "excessive page size", "before query", "after query", "deadline", "query error with cancellation", "before return"} {
		t.Run(phase, func(t *testing.T) {
			actor, page := userSessionServiceFixture()
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
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 20*time.Millisecond)
				defer stop()
				expectedCalls, want = 1, context.DeadlineExceeded
			}
			if phase == "before return" {
				ctx = &cancelBeforeSessionListReturnContext{Context: ctx, cancel: cancel}
			}
			queryCalls := 0
			sessions := NewUserSessionService(func(received context.Context, _ uint64, _ uint64, _ int) (*domain.UserSessionPage, error) {
				queryCalls++
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
			result, err := sessions.List(ctx, actor, 0, pageSize)
			if result != nil || err == nil || queryCalls != expectedCalls || (want != nil && !errors.Is(err, want)) {
				t.Fatal("参数或取消未按预期结束处理", err)
			}
		})
	}
}

// cancelBeforeSessionListReturnContext 在结果复制完成后的第三次检查时取消。
type cancelBeforeSessionListReturnContext struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (ctx *cancelBeforeSessionListReturnContext) Err() error {
	ctx.checks++
	if ctx.checks == 3 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestUserSessionServiceDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少查询方法仍构造成功")
		}
	}()
	NewUserSessionService(nil)
}
