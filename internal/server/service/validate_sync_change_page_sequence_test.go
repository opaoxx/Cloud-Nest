package service

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestValidateSyncChangePageSequence(t *testing.T) {
	// 1. 准备完整页、末页、空页，以及各种异常的仓储返回。
	fullPage := make([]uint64, 100)
	for index := range fullPage {
		fullPage[index] = uint64(index) + 1
	}
	for _, scenario := range []struct {
		name                string
		position, highWater uint64
		pageSize            int
		sequences           []uint64
		invalid             bool
	}{
		{name: "first page from zero", highWater: 4, pageSize: 2, sequences: []uint64{1, 2}},
		{name: "single event page", position: 17, highWater: 22, pageSize: 1, sequences: []uint64{18}},
		{name: "full size of one hundred", highWater: 101, pageSize: 100, sequences: fullPage},
		{name: "short final page", position: 20, highWater: 22, pageSize: 5, sequences: []uint64{21, 22}},
		{name: "empty page", position: 22, highWater: 22, pageSize: 2},
		{name: "empty log", pageSize: 1},
		{name: "empty at maximum", position: math.MaxUint64, highWater: math.MaxUint64, pageSize: 1},
		{name: "ends at maximum", position: math.MaxUint64 - 2, highWater: math.MaxUint64, pageSize: 5, sequences: []uint64{math.MaxUint64 - 1, math.MaxUint64}},
		{name: "remaining range exceeds signed int", highWater: math.MaxUint64, pageSize: 1, sequences: []uint64{1}},
		{name: "missing page", position: 17, highWater: 22, pageSize: 2, invalid: true},
		{name: "missing tail", position: 17, highWater: 22, pageSize: 2, sequences: []uint64{18}, invalid: true},
		{name: "missing first with full count", position: 17, highWater: 22, pageSize: 2, sequences: []uint64{19, 20}, invalid: true},
		{name: "internal gap with full count", position: 17, highWater: 22, pageSize: 2, sequences: []uint64{18, 20}, invalid: true},
		{name: "duplicate sequence", position: 17, highWater: 22, pageSize: 2, sequences: []uint64{18, 18}, invalid: true},
		{name: "out of order", position: 17, highWater: 22, pageSize: 2, sequences: []uint64{19, 18}, invalid: true},
		{name: "includes start", position: 17, highWater: 22, pageSize: 2, sequences: []uint64{17, 18}, invalid: true},
		{name: "exceeds high water", position: 20, highWater: 22, pageSize: 2, sequences: []uint64{21, 23}, invalid: true},
		{name: "zero event sequence", highWater: 1, pageSize: 1, sequences: []uint64{0}, invalid: true},
		{name: "extra event over limit", position: 17, highWater: 22, pageSize: 2, sequences: []uint64{18, 19, 20}, invalid: true},
		{name: "unexpected event on empty page", position: 22, highWater: 22, pageSize: 2, sequences: []uint64{23}, invalid: true},
		{name: "start above high water", position: 23, highWater: 22, pageSize: 2, invalid: true},
		{name: "negative page size", pageSize: -1, invalid: true},
		{name: "zero page size", pageSize: 0, invalid: true},
		{name: "page size over maximum", pageSize: 101, invalid: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			events := make([]domain.SyncChangeEvent, len(scenario.sequences))
			for index, sequence := range scenario.sequences {
				events[index] = domain.SyncChangeEvent{Sequence: sequence}
			}
			before := append([]domain.SyncChangeEvent{}, events...)

			// 2. 检查整页结论，并确认原始顺序和字段未被改写。
			err := ValidateSyncChangePageSequence(scenario.position, scenario.highWater, scenario.pageSize, events)
			if scenario.invalid {
				if !errors.Is(err, domain.ErrInvalidSyncChangePage) {
					t.Fatalf("期望增量页异常，实际为 %v", err)
				}
			} else if err != nil {
				t.Fatalf("完整事件页校验失败：%v", err)
			}
			if !reflect.DeepEqual(events, before) {
				t.Fatalf("校验改变了原始事件：%+v", events)
			}
		})
	}
}
