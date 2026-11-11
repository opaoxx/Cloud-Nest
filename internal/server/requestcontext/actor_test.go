package requestcontext

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestActorPreservesParentContext(t *testing.T) {
	// 1. 创建包含其他值、请求 ID 和期限的父上下文。
	type otherContextKey struct{}
	parent, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	parent = context.WithValue(parent, otherContextKey{}, "original-value")
	parent = WithRequestID(parent, "request-038")
	want := domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}
	child := WithActor(parent, want)

	// 2. 子上下文保存身份，并保留父上下文的值、期限和取消通道。
	actor, exists := Actor(child)
	if !exists || actor != want || RequestID(child) != "request-038" || child.Value(otherContextKey{}) != "original-value" {
		t.Fatal("附加身份丢失原有上下文值")
	}
	if parentActor, exists := Actor(parent); exists || parentActor != (domain.Actor{}) {
		t.Fatal("保存身份修改了父上下文")
	}
	parentDeadline, _ := parent.Deadline()
	childDeadline, hasDeadline := child.Deadline()
	if !hasDeadline || !childDeadline.Equal(parentDeadline) || child.Done() != parent.Done() {
		t.Fatal("保存身份丢失期限或取消通道")
	}
	if actor, exists := Actor(WithRequestID(child, "request-038-updated")); !exists || actor != want {
		t.Fatal("附加请求 ID 丢失已有身份")
	}

	// 3. 父上下文取消后，子上下文保留取消原因与已有身份。
	cancel()
	select {
	case <-child.Done():
		if child.Err() != context.Canceled {
			t.Fatal("取消原因没有传到身份上下文")
		}
	case <-time.After(time.Second):
		t.Fatal("身份上下文未接收到取消信号")
	}
	if actor, exists := Actor(child); !exists || actor != want {
		t.Fatal("取消丢失了请求身份")
	}
}

func TestActorValueCopiesAndChildIsolation(t *testing.T) {
	original := domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}
	want := original
	parent := WithActor(context.Background(), original)
	original.UserID = 99
	firstRead, exists := Actor(parent)
	if !exists || firstRead != want {
		t.Fatal("修改输入身份影响了保存的值")
	}
	firstRead.SessionID = 99
	if secondRead, exists := Actor(parent); !exists || secondRead != want {
		t.Fatal("修改读取结果影响了保存的值")
	}
	other := domain.Actor{UserID: math.MaxUint64, SessionID: math.MaxUint64, DeviceID: math.MaxUint64}
	child := WithActor(parent, other)
	if actor, exists := Actor(child); !exists || actor != other {
		t.Fatal("子上下文未完整保存新的 uint64 身份")
	}
	if actor, exists := Actor(parent); !exists || actor != want {
		t.Fatal("子上下文身份影响了父上下文")
	}
}

func TestActorMissingAndKeyIsolation(t *testing.T) {
	type otherActorKey struct{}
	for _, testCase := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "nil"},
		{name: "missing", ctx: context.Background()},
		{name: "string key", ctx: context.WithValue(context.Background(), "actor", domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13})},
		{name: "other typed key", ctx: context.WithValue(context.Background(), otherActorKey{}, domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13})},
		{name: "wrong value type", ctx: context.WithValue(context.Background(), actorKey{}, "wrong-type")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actor, exists := Actor(testCase.ctx)
			if exists || actor != (domain.Actor{}) {
				t.Fatal("缺失或类型不匹配的身份应返回零值与 false")
			}
		})
	}
}

func TestActorRejectsInvalidInput(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		parent context.Context
		actor  domain.Actor
	}{
		{name: "nil parent", actor: domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}},
		{name: "zero identity", parent: context.Background()},
		{name: "zero user", parent: context.Background(), actor: domain.Actor{SessionID: 11, DeviceID: 13}},
		{name: "zero session", parent: context.Background(), actor: domain.Actor{UserID: 7, DeviceID: 13}},
		{name: "zero device", parent: context.Background(), actor: domain.Actor{UserID: 7, SessionID: 11}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("无效输入应明确暴露调用错误")
				}
			}()
			WithActor(testCase.parent, testCase.actor)
		})
	}
}

func TestActorDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	child := WithActor(parent, domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13})
	select {
	case <-child.Done():
		if child.Err() != context.DeadlineExceeded {
			t.Fatal("到期原因没有传到身份上下文")
		}
	case <-time.After(time.Second):
		t.Fatal("身份上下文未按父期限到期")
	}
}

func TestActorConcurrentIsolation(t *testing.T) {
	// 32 个子请求共享同一父上下文，各自保存身份并修改本地读取副本。
	parent := WithRequestID(context.Background(), "shared-request-id")
	var requests sync.WaitGroup
	for requestNumber := uint64(1); requestNumber <= 32; requestNumber++ {
		requests.Go(func() {
			want := domain.Actor{UserID: requestNumber, SessionID: requestNumber + 100, DeviceID: requestNumber + 200}
			child := WithActor(parent, want)
			actor, exists := Actor(child)
			if !exists || actor != want || RequestID(child) != "shared-request-id" {
				t.Error("并发请求身份或已有值出现串号")
			}
			actor.UserID = 99
			if stored, exists := Actor(child); !exists || stored != want {
				t.Error("修改副本影响了并发请求身份")
			}
		})
	}
	requests.Wait()
	if actor, exists := Actor(parent); exists || actor != (domain.Actor{}) {
		t.Fatal("并发子请求向父上下文泄漏了身份")
	}
}
