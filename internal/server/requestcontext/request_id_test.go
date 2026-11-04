package requestcontext

import (
	"context"
	"testing"
	"time"
)

func TestRequestIDPreservesParentContext(t *testing.T) {
	// 1. 父上下文已有取消、期限和其他包定义的值，附加 ID 后都应保留。
	type otherContextKey struct{}
	parent, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	parent = context.WithValue(parent, otherContextKey{}, "original-value")
	child := WithRequestID(parent, "28ee758a-0727-42de-ae7d-3bb33155e148")
	parentDeadline, _ := parent.Deadline()
	childDeadline, exists := child.Deadline()
	if RequestID(parent) != "" || RequestID(child) != "28ee758a-0727-42de-ae7d-3bb33155e148" || child.Value(otherContextKey{}) != "original-value" || !exists || !parentDeadline.Equal(childDeadline) {
		t.Fatal("附加 ID 时修改了父上下文或丢失已有信息")
	}

	// 2. 父上下文取消后，附加 ID 的上下文也必须及时取消。
	cancel()
	select {
	case <-child.Done():
		if child.Err() != context.Canceled {
			t.Fatal("取消原因不符合预期")
		}
	case <-time.After(time.Second):
		t.Fatal("取消通知没有传递到子上下文")
	}
}
