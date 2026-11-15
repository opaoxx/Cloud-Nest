package service

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestValidateEntryRevisionMatching(t *testing.T) {
	for _, revision := range []uint64{1, 3, 1<<53 - 1, 1 << 53, 1<<53 + 1, 1 << 63, ^uint64(0) - 1, ^uint64(0)} {
		if err := ValidateEntryRevision(revision, revision); err != nil {
			t.Fatal("相同有效版本未通过", revision, err)
		}
	}
}

func TestValidateEntryRevisionConflicts(t *testing.T) {
	for _, testCase := range []struct {
		current, expected uint64
	}{
		{3, 2}, {3, 4}, {1, ^uint64(0)}, {^uint64(0), 1},
		{1<<53 + 1, 1 << 53}, {1 << 53, 1<<53 + 1}, {1 << 63, 1<<63 - 1},
		{^uint64(0), ^uint64(0) - 1}, {^uint64(0) - 1, ^uint64(0)},
	} {
		err := ValidateEntryRevision(testCase.current, testCase.expected)
		if !errors.Is(err, domain.ErrEntryRevisionConflict) || err.Error() != "文件已发生变化，请刷新后重试" {
			t.Fatal("版本不一致未返回固定冲突类别", err)
		}
		wrapped := fmt.Errorf("fixture outer operation: %w", err)
		var conflict *domain.EntryRevisionConflictError
		if !errors.Is(wrapped, domain.ErrEntryRevisionConflict) || !errors.As(wrapped, &conflict) || conflict.CurrentRevision != testCase.current {
			t.Fatal("包装后无法取得冲突类别或完整当前版本", wrapped)
		}
		if errors.Is(err, domain.ErrInvalidFileEntryData) {
			t.Fatal("正常版本冲突被混为异常数据")
		}
	}
}

func TestValidateEntryRevisionInvalid(t *testing.T) {
	if err := ValidateEntryRevision(0, 1); !errors.Is(err, domain.ErrInvalidFileEntryData) || errors.Is(err, domain.ErrEntryRevisionConflict) {
		t.Fatal("数据库零版本未作为数据异常处理", err)
	}
	for _, current := range []uint64{0, 1, ^uint64(0)} {
		err := ValidateEntryRevision(current, 0)
		if err == nil || err.Error() != "校验目录项版本需要非零预期版本" || errors.Is(err, domain.ErrEntryRevisionConflict) || errors.Is(err, domain.ErrInvalidFileEntryData) {
			t.Fatal("零预期版本的调用错误或优先顺序错误", err)
		}
	}
}

func TestValidateEntryRevisionIndependent(t *testing.T) {
	current, expected := uint64(3), uint64(2)
	first := ValidateEntryRevision(current, expected)
	second := ValidateEntryRevision(current, expected)
	var firstConflict, secondConflict *domain.EntryRevisionConflictError
	if !errors.As(first, &firstConflict) || !errors.As(second, &secondConflict) || firstConflict == secondConflict {
		t.Fatal("不同校验调用未返回独立冲突数据")
	}
	firstConflict.CurrentRevision = 99
	if secondConflict.CurrentRevision != 3 || current != 3 || expected != 2 {
		t.Fatal("修改冲突数据影响其他调用或原始版本")
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				maximum := ^uint64(0)
				if err := ValidateEntryRevision(maximum, maximum); err != nil {
					t.Error("并发相同版本校验失败", err)
					return
				}
				var conflict *domain.EntryRevisionConflictError
				if err := ValidateEntryRevision(maximum, maximum-1); !errors.As(err, &conflict) || conflict.CurrentRevision != maximum {
					t.Error("并发冲突版本不准确", err)
					return
				}
			}
		}()
	}
	workers.Wait()
}
