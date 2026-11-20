package domain

import (
	"math"
	"testing"
	"time"
)

func TestTrashEntryPositionIsValid(t *testing.T) {
	// 1. 准备普通回收时间、UTC 时间上下界和跨年用的时区。
	deletedAt := time.Date(2026, 11, 20, 1, 30, 0, 123456000, time.UTC)
	minimumTime := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
	maximumTime := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	eastZone := time.FixedZone("UTC+08", 8*60*60)
	westZone := time.FixedZone("UTC-08", -8*60*60)

	// 2. 核对正常值、时间精度和 UTC 边界。
	for _, testCase := range []struct {
		name     string
		position TrashEntryPosition
		valid    bool
	}{
		{name: "zero position"},
		{name: "zero time", position: TrashEntryPosition{ID: 1}},
		{name: "zero ID", position: TrashEntryPosition{DeletedAt: deletedAt}},
		{name: "minimum ID", position: TrashEntryPosition{DeletedAt: deletedAt, ID: 1}, valid: true},
		{name: "maximum ID", position: TrashEntryPosition{DeletedAt: deletedAt, ID: math.MaxUint64}, valid: true},
		{name: "whole second", position: TrashEntryPosition{DeletedAt: deletedAt.Truncate(time.Second), ID: 12}, valid: true},
		{name: "extra nanosecond", position: TrashEntryPosition{DeletedAt: deletedAt.Add(time.Nanosecond), ID: 12}},
		{name: "UTC lower bound", position: TrashEntryPosition{DeletedAt: minimumTime, ID: 12}, valid: true},
		{name: "UTC upper bound", position: TrashEntryPosition{DeletedAt: maximumTime, ID: 12}, valid: true},
		{name: "before UTC lower bound", position: TrashEntryPosition{DeletedAt: minimumTime.Add(-time.Microsecond), ID: 12}},
		{name: "after UTC upper bound", position: TrashEntryPosition{DeletedAt: maximumTime.Add(time.Microsecond), ID: 12}},
		{name: "UTC lower bound shown as year 999", position: TrashEntryPosition{DeletedAt: minimumTime.In(westZone), ID: 12}, valid: true},
		{name: "UTC upper bound shown as year 10000", position: TrashEntryPosition{DeletedAt: maximumTime.In(eastZone), ID: 12}, valid: true},
		{name: "local year 1000 with invalid UTC", position: TrashEntryPosition{DeletedAt: time.Date(1000, 1, 1, 0, 0, 0, 0, eastZone), ID: 12}},
		{name: "local year 9999 with invalid UTC", position: TrashEntryPosition{DeletedAt: time.Date(9999, 12, 31, 23, 59, 59, 0, westZone), ID: 12}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if valid := testCase.position.IsValid(); valid != testCase.valid {
				t.Fatalf("位置 %+v 的校验结果为 %v，期望 %v", testCase.position, valid, testCase.valid)
			}
		})
	}
}
